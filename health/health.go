package health

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/go-nats"
)

// sysMemberPrefix creates a namespace
// for system cluster membership communication.
// This prefix aims to avoid collisions
// with user-level topics. Only system
// processes / internal clients should
// write to these topics, but everyone
// is welcome to listen on them.
//
// note: `_nats` is for now, can easily
// changed to be `_SYS` later once
// we're sure everything is working.
//
const sysMemberPrefix = "_nats.cluster.members."

// ServerLoc conveys to interested parties
// the Id and location of one gnatsd
// server in the cluster.
type ServerLoc struct {
	Id   string `json:"serverId"`
	Host string `json:"host"`
	Port int    `json:"port"`

	// Are we the leader?
	IsLeader bool `json:"leader"`

	// LeaseExpires is zero for any
	// non-leader. For the leader,
	// LeaseExpires tells you when
	// the leaders lease expires.
	LeaseExpires time.Time `json:"leaseExpires"`

	// lower rank is leader until lease
	// expires. Ties are broken by Id.
	// Rank should be assignable on the
	// gnatsd command line with -rank to
	// let the operator prioritize
	// leadership for certain hosts.
	Rank int `json:"rank"`
}

func (s *ServerLoc) String() string {
	by, err := json.Marshal(s)
	panicOn(err)
	return string(by)
}

func (s *ServerLoc) fromBytes(by []byte) error {
	return json.Unmarshal(by, s)
}

// Membership tracks the nats server cluster
// membership, issuing health checks and
// choosing a leader.
type Membership struct {
	Cfg MembershipCfg

	// the pongCollector holds
	// all the pongs received in
	// response to allcall pings
	// in the most recent heartbeat
	// session.
	pc *pongCollector

	// actually elected leaders, should
	// change only after a lease term.
	elec  *leadHolder
	nc    *nats.Conn
	myLoc ServerLoc

	subjAllCall     string
	subjAllReply    string
	subjMemberLost  string
	subjMemberAdded string
	subjMembership  string

	halt     *halter
	mu       sync.Mutex
	stopping bool

	needReconnect chan bool
}

func (m *Membership) deaf() bool {
	v := atomic.LoadInt64(&m.Cfg.deaf)
	return v == DEAF_TRUE
}

func (m *Membership) setDeaf() {
	atomic.StoreInt64(&m.Cfg.deaf, DEAF_TRUE)
}

func (m *Membership) unDeaf() {
	atomic.StoreInt64(&m.Cfg.deaf, DEAF_FALSE)
}

func NewMembership(cfg *MembershipCfg) *Membership {
	m := &Membership{
		Cfg:  *cfg,
		halt: newHalter(),

		// needReconnect should be sent on, not closed.
		needReconnect: make(chan bool),
	}
	m.pc = m.newPongCollector()
	m.elec = m.newLeadHolder(cfg.historyCount)
	return m
}

// leadHolder holds who is the current leader,
// and what their lease is. Used to synchronize
// access between various goroutines.
type leadHolder struct {
	mu   sync.Mutex
	sloc ServerLoc

	myId            string
	myRank          int
	myLocHasBeenSet bool

	history *RingBuf
	histsz  int

	m *Membership
}

func (m *Membership) newLeadHolder(histsz int) *leadHolder {
	if histsz == 0 {
		histsz = 100
	}
	return &leadHolder{
		history: NewRingBuf(histsz),
		histsz:  histsz,
		m:       m,
	}
}

func (e *leadHolder) setMyLoc(myLoc *ServerLoc) {
	e.mu.Lock()
	if e.myLocHasBeenSet {
		panic("no double set!")
	}
	e.myLocHasBeenSet = true
	e.myId = myLoc.Id
	e.myRank = myLoc.Rank
	e.mu.Unlock()
}

// getLeader retreives the stored e.sloc value.
func (e *leadHolder) getLeader() ServerLoc {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sloc
}

// setLeader aims to copy sloc and store it
// for future getLeader() calls to access.
//
// However we reject any attempt to replace
// a leader with a one that doesn't rank lower, where rank
// includes the LeaseExpires time
// (see the ServerLocLessThan() function).
//
// If we accept sloc
// we return slocWon true. If we reject sloc then
// we return slocWon false. In short, we will only
// accept sloc if ServerLocLessThan(sloc, e.sloc),
// and we return ServerLocLessThan(sloc, e.sloc).
//
// If we return slocWon false, alt contains the
// value we favored, which is the current value
// of our retained e.sloc. If we return true,
// then alt contains a copy of sloc. We
// return a value in alt to avoid data races.
//
func (e *leadHolder) setLeader(sloc *ServerLoc) (slocWon bool, alt ServerLoc) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if sloc == nil || sloc.Id == "" {
		//p("port %v, 77777 setLeader is returning false because sloc==nil or sloc.Id==empty string", e.m.myLoc.Port)
		return false, e.sloc
	}

	//now := time.Now().UTC()
	newWon := ServerLocLessThan(sloc, &e.sloc)
	oldWon := ServerLocLessThan(&e.sloc, sloc)
	switch {
	case !newWon && !oldWon:
		// they are equal, pick the longer lease
		// so we allow lease renewal
		if sloc.LeaseExpires.After(e.sloc.LeaseExpires) {
			slocWon = true
			alt = *sloc
			e.sloc = *sloc

			//p("%v, port %v, 999999 setLeader sees same leader with > lease, renewing its lease %v\n", now, e.m.myLoc.Port, &e.sloc)
		} else {
			slocWon = false
			alt = e.sloc

			//p("%v, port %v, 000000 setLeader is failing to update the leader, rejecting the new contendor.\n\nsloc='%s'\n >= \n prev:'%s'\n", now, e.m.myLoc.Port, sloc, &e.sloc)
		}
	case newWon:
		slocWon = true
		alt = *sloc
		e.sloc = *sloc
	default:
		//oldWon
		slocWon = false
		alt = e.sloc
	}

	// update history
	if slocWon {
		histcp := *sloc
		e.history.Append(&histcp)
	}

	return
}

func (e *leadHolder) getLeaderAsBytes() []byte {
	lead := e.getLeader()
	by, err := json.Marshal(&lead)
	panicOn(err)
	return by
}

// Stop blocks until the Membership goroutine
// acknowledges the shutdown request.
func (m *Membership) Stop() {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	m.stopping = true
	m.mu.Unlock()
	m.halt.ReqStop.Close()
	<-m.halt.Done.Chan
}

func (m *Membership) Start() error {

	m.Cfg.SetDefaults()

	err := m.setupNatsClient()
	if err != nil {
		m.halt.Done.Close()
		return err
	}
	go m.start()
	return nil
}

func (m *Membership) start() {

	var nc *nats.Conn = m.nc
	var pc *pongCollector = m.pc

	defer func() {
		m.halt.Done.Close()
	}()

	m.Cfg.Log.Debugf("health-agent: Listening on [%s]\n", m.subjAllCall)

	prevCount, curCount := 0, 0
	var curMember, prevMember *members
	var curLead *ServerLoc

	// do an initial allcall() to discover any
	// current leader.
	m.Cfg.Log.Debugf("health-agent: " +
		"init: doing initial allcall " +
		"to discover any existing leader...")

	err := m.allcall()
	if err != nil {
		m.Cfg.Log.Debugf("health-agent: "+
			"error back from allcall, "+
			"terminating on: %s", err)
		return
	}

	select {
	case <-time.After(m.Cfg.BeatDur):
		// continue below, initial heartbeat done.
	case <-m.needReconnect:
		err := m.setupNatsClient()
		if err != nil {
			m.Cfg.Log.Debugf("health-agent: "+
				"fatal error: could not reconnect to, "+
				"our url '%s', error: %s",
				m.Cfg.NatsUrl, err)

			m.halt.ReqStop.Close()
			return
		}
	case <-m.halt.ReqStop.Chan:
		return
	}

	prevCount, prevMember = pc.getSetAndClear(m.myLoc)
	now := time.Now().UTC()
	p("%v, 0-th round, myLoc:%s, prevMember='%s'", now, &m.myLoc, prevMember)

	firstSeenLead := m.elec.getLeader()
	xpire := firstSeenLead.LeaseExpires

	limit := xpire.Add(m.Cfg.MaxClockSkew)
	if !xpire.IsZero() && limit.After(now) {

		m.Cfg.Log.Debugf("health-agent: init: "+
			"after one heartbeat, "+
			"we detect current leader '%s'"+
			" of rank %v with lease good "+
			"for %v until expiration + "+
			"maxClockSkew=='%v'",
			firstSeenLead.Id,
			firstSeenLead.Rank,
			limit.Sub(now),
			limit,
		)
	} else {
		m.Cfg.Log.Tracef("health-agent: "+
			"init: after one heartbeat,"+
			" no leader found. waiting "+
			"for a full leader lease "+
			"term of %s to expire...",
			m.Cfg.LeaseTime)

		select {
		case <-time.After(m.Cfg.LeaseTime):
		case <-m.needReconnect:
			err := m.setupNatsClient()
			if err != nil {
				m.Cfg.Log.Debugf("health-agent: "+
					"fatal error: could not reconnect to, "+
					"our url '%s', error: %s",
					m.Cfg.NatsUrl, err)

				m.halt.ReqStop.Close()
				return
			}
		case <-m.halt.ReqStop.Chan:
			return
		}
	}

	// prev responses should be back by now.
	var expired bool
	var prevLead *ServerLoc
	var nextLeadReportTm time.Time

	k := 0
	for {
		k++
		// NB: replies to an
		// allcall will only update
		// the pongCollectors.from set,
		// and won't change
		// what the current leader
		// is in elec.
		p("%v, port %v, issuing k-th (k=%v) allcall", time.Now().UTC(), m.myLoc.Port, k)
		err = m.allcall()
		if err != nil {
			// err could be: "write on closed buffer"
			// typically means we are shutting down.

			m.Cfg.Log.Tracef("health-agent: "+
				"error on allcall, "+
				"shutting down the "+
				"health-agent: %s",
				err)
			return
		}

		//p("%v, port %v, SLEEPING for a heartbeat of %v", time.Now().UTC(), m.myLoc.Port, m.Cfg.BeatDur)
		select {
		case <-time.After(m.Cfg.BeatDur):
			// continue below, latest heartbeat session done.
		case <-m.needReconnect:
			err := m.setupNatsClient()
			if err != nil {
				m.Cfg.Log.Debugf("health-agent: "+
					"fatal error: could not reconnect to, "+
					"our url '%s', error: %s",
					m.Cfg.NatsUrl, err)

				m.halt.ReqStop.Close()
				return
			}
		case <-m.halt.ReqStop.Chan:
			return
		}
		lastSeenLead := m.elec.getLeader()

		// cur responses should be back by now
		// and we can compare prev and cur.
		curCount, curMember = pc.getSetAndClear(m.myLoc)
		now = time.Now().UTC()
		//p("%v, port %v, k-th (k=%v) round conclusion, curMember='%s'", now, m.myLoc.Port, k, curMember)

		expired, curLead = curMember.leaderLeaseCheck(
			now,
			m.Cfg.LeaseTime,
			&lastSeenLead,
			m.Cfg.MaxClockSkew,
			m,
		)

		// record in our history
		won, alt := m.elec.setLeader(curLead)
		if !won {
			//p("%v, port %v, k-th (k=%v) round conclusion of trying to setLeader: rejected '%s' in favor of '%s'", now, m.myLoc.Port, k, curLead, &alt)
			curLead = &alt
		} else {
			//p("%v, port %v, k-th (k=%v) round conclusion of trying to setLeader: accepted as new lead '%s'", now, m.myLoc.Port, k, curLead)
		}

		loc, _ := m.getNatsServerLocation()
		if loc != nil {
			if loc.Id == curLead.Id {

				if now.After(nextLeadReportTm) ||
					prevLead == nil ||
					prevLead.Id != curLead.Id {

					left := curLead.LeaseExpires.Sub(now)
					m.Cfg.Log.Debugf("health-agent: "+
						"I am LEAD, my Id: '%s', "+
						"rank %v port %v. lease expires "+
						"in %s",
						loc.Id,
						loc.Rank,
						loc.Port,
						left)

					nextLeadReportTm = now.Add(left).Add(time.Second)
				}
			} else {
				if prevLead != nil &&
					prevLead.Id == loc.Id {

					m.Cfg.Log.Debugf("health-agent: "+
						"I am no longer lead, "+
						"new LEAD is '%s', rank %v. "+
						"port %v. lease expires in %s",
						curLead.Id,
						curLead.Rank,
						curLead.Port,
						curLead.LeaseExpires.Sub(now))

				} else {
					if curLead != nil &&
						(nextLeadReportTm.IsZero() ||
							now.After(nextLeadReportTm)) {

						left := curLead.LeaseExpires.Sub(now)
						if curLead.Id == "" {
							m.Cfg.Log.Debugf("health-agent: "+
								"I am '%s'/rank=%v. "+
								"port %v. lead is unknown.",
								m.myLoc.Id,
								m.myLoc.Rank,
								m.myLoc.Port)

						} else {
							m.Cfg.Log.Debugf("health-agent: "+
								"I am not lead. lead is '%s', "+
								"rank %v, port %v, for %v",
								curLead.Id,
								curLead.Rank,
								curLead.Port,
								left)

						}
						nextLeadReportTm = now.Add(left).Add(time.Second)
					}
				}
			}
		}

		lost := setDiff(prevMember, curMember, curLead)
		gained := setDiff(curMember, prevMember, curLead)
		same := setsEqual(prevMember, curMember)

		if same {
			// nothing more to do.
			// This is the common case when nothing changes.
		} else {
			lostBytes := lost.mustJsonBytes()
			if !lost.setEmpty() {
				if !m.deaf() {
					nc.Publish(m.subjMemberLost, lostBytes)
					// ignore errors on purpose;
					// don't crash mid-health-report
					// if at all possible.
				}
			}
			gainedBytes := gained.mustJsonBytes()
			if !gained.setEmpty() {
				if !m.deaf() {
					nc.Publish(m.subjMemberAdded, gainedBytes)
					// same error approach as above.
				}
			}
		}
		if curCount < prevCount {
			m.Cfg.Log.Debugf("health-agent: ---- "+
				"PAGE PAGE PAGE!! we went "+
				"down a server, from %v -> %v."+
				"lost: '%s'",
				prevCount,
				curCount,
				lost)

		} else if curCount > prevCount && prevCount > 0 {
			m.Cfg.Log.Debugf("health-agent: ++++  "+
				"MORE ROBUSTNESS GAINED; "+
				"we went from %v -> %v. "+
				"gained: '%s'",
				prevCount,
				curCount,
				gained)

		}

		if expired {
			curBytes := curMember.mustJsonBytes()
			if !m.deaf() {
				nc.Publish(m.subjMembership, curBytes)
			}
		}

		// done with compare, now loop
		prevCount = curCount
		prevMember = curMember
		prevLead = curLead
	}
}

func pong(nc *nats.Conn, subj string, msg []byte) {
	err := nc.Publish(subj, msg)
	panicOn(err)
	nc.Flush()
	//nc.FlushTimeout(2 * time.Second)
	// ignore error on nc.Flush().
	// might be: nats: connection closed on shutdown.
}

// allcall sends out a health ping on the
// subjAllCall topic.
//
// The ping consists of sending the ServerLoc
// forf the current leader, which provides lease
// and full contact info for the leader.
//
// This gives a round-trip connectivity check.
//
func (m *Membership) allcall() error {
	// allcall broadcasts the current leader + lease
	leadby := m.elec.getLeaderAsBytes()
	return m.nc.PublishRequest(m.subjAllCall, m.subjAllReply, leadby)
}

// pongCollector collects the responses
// from an allcall request.
type pongCollector struct {
	replies int
	from    *members
	mu      sync.Mutex
	mship   *Membership
}

func (m *Membership) newPongCollector() *pongCollector {
	return &pongCollector{
		from:  newMembers(),
		mship: m,
	}
}

// acumulate pong responses
func (pc *pongCollector) receivePong(msg *nats.Msg) {
	pc.mu.Lock()

	pc.replies++

	var loc ServerLoc
	err := loc.fromBytes(msg.Data)
	if err == nil {
		pc.from.DedupTree.insert(&loc)
	} else {
		panic(err)
	}

	//p("%v, port %v, pong collector received '%#v'. pc.from is now '%s'", time.Now().UTC(), pc.mship.myLoc.Port, &loc, pc.from)

	pc.mu.Unlock()
}

func (pc *pongCollector) clear() {
	pc.mu.Lock()
	pc.from.clear()
	pc.mu.Unlock()
}

// getSet returns the count and set so far, then
// clears the set, emptying it, and then adding
// back just myLoc
func (pc *pongCollector) getSetAndClear(myLoc ServerLoc) (int, *members) {

	mem := pc.from.clone()
	pc.clear()

	// add myLoc to pc.from as a part of "reset"
	pc.from.DedupTree.insert(&myLoc)

	p("%v, in getSetAndClear, here are the contents of mem.DedupTree: '%s'", time.Now().UTC(), mem.DedupTree)

	// return the old member set
	return mem.DedupTree.Len(), mem
}

// leaderLeaseCheck evaluates the lease as of now,
// and returns the leader or best candiate. Returns
// expired == true if any prior leader lease has
// lapsed. In this case we return the best new
// leader with its IsLeader bit set and its
// LeaseExpires set to now + lease.
//
// If expired == false then the we return
// the current leader in lead.
//
// PRE: there are only 0 or 1 leaders in m.DedupTree
//      who have a non-zero LeaseExpires field.
//
// If m.DedupTree is empty, we return (true, nil).
//
// This method is where the actual "election"
// happens. See the ServerLocLessThan()
// function below for exactly how
// we rank candidates.
//
func (m *members) leaderLeaseCheck(
	now time.Time,
	leaseLen time.Duration,
	prevLead *ServerLoc,
	maxClockSkew time.Duration,
	mship *Membership,

) (expired bool, lead *ServerLoc) {

	if prevLead.LeaseExpires.Add(maxClockSkew).After(now) {
		// honor the leases until they expire
		//p("leaderLeaseCheck: honoring outstanding lease")
		return false, prevLead
	}

	if m.DedupTree.Len() == 0 {
		p("leaderLeaseCheck: m.DedupTree.Len is 0")
		return false, prevLead
	}

	// INVAR: any lease has expired.

	lead = m.DedupTree.minrank()
	lead.IsLeader = true
	lead.LeaseExpires = now.Add(leaseLen).UTC()

	// debug:
	//	if mship.Cfg.MyRank == 1 {
	//		p("port %v, leaderLeaseCheck has list of len %v:",
	//			mship.myLoc.Port, len(sortme)) // TODO: racy read of mship.myLoc
	//		for i := range sortme {
	//			fmt.Printf("port %v, sortme[%v]=%v\n", mship.myLoc.Port, i, sortme[i])
	//		}
	//		fmt.Printf("\n\n")
	//	}
	return true, lead
}

type byRankThenId struct {
	s   []*ServerLoc
	now time.Time
}

func (p byRankThenId) Len() int      { return len(p.s) }
func (p byRankThenId) Swap(i, j int) { p.s[i], p.s[j] = p.s[j], p.s[i] }

// Less must be stable and computable locally yet
// applicable globally: it is how we choose a leader
// in a stable fashion.
func (p byRankThenId) Less(i, j int) bool {
	return ServerLocLessThan(p.s[i], p.s[j])
}

// ServerLocLessThan returns true iff i < j, in
// terms of leader preference where lowest is
// more electable/preferred as leader.
func ServerLocLessThan(i, j *ServerLoc) bool {

	/*
		nowu := now.UnixNano()
		itm := i.LeaseExpires.UnixNano()
		jtm := j.LeaseExpires.UnixNano()

		// if both are in force, then
		// we'll go ahead and force a
		// choice based on the next
		// criteria; rank, id, host, port.
		// This speeds up convergence
		// massively for the 102 test
		// of healing after split brain.

		// if both are expired,
		// or both in force, then its a tie.
		if jtm <= nowu {
			jtm = 0
		}
		if itm <= nowu {
			itm = 0
		}

		if (itm == 0 &&
			jtm == 0) ||
			(itm > nowu &&
				jtm > nowu) {
			// its a tie on leases
			// proceed to rank
		} else {
			// we want an actual time to sort before a zero-time.
			return itm > jtm
		}
	*/

	// recognize empty ServerLoc and sort them high, not low.
	iempt := i.Id == ""
	jempt := j.Id == ""
	if iempt && jempt {
		return false // "" == ""
	}
	if jempt {
		return true // "123" < ""
	}
	if iempt {
		return false // "" > "123"
	}

	if i.Rank != j.Rank {
		return i.Rank < j.Rank
	}
	if i.Id != j.Id {
		return lessThanString(i.Id, j.Id)
	}
	if i.Host != j.Host {
		return lessThanString(i.Host, j.Host)
	}
	return i.Port < j.Port
}

// return i < j where empty strings are big not small.
func lessThanString(i, j string) bool {
	iempt := i == ""
	jempt := j == ""
	if iempt || jempt {
		if jempt {
			return true // "123" < ""
		}
		return false
	}
	return i < j
}

func (m *Membership) setupNatsClient() error {
	var pc *pongCollector = m.pc

	discon := func(nc *nats.Conn) {
		select {
		case m.needReconnect <- true:
		case <-m.halt.ReqStop.Chan:
			return
		}
	}
	optdis := nats.DisconnectHandler(discon)
	norand := nats.DontRandomize()

	// We don't want to get connected to
	// some different server in the pool,
	// so any reconnect, if needed, will
	// need to be handled manually by us by
	// attempting to contact the
	// exact same address as we are
	// configured with; see the m.needReconnect
	// channel.
	// Otherwise we are monitoring
	// the health of the wrong server.
	//
	optrecon := nats.NoReconnect()

	opts := []nats.Option{optdis, optrecon, norand}
	if m.Cfg.CliConn != nil {
		opts = append(opts, nats.Dialer(&m.Cfg))
	}

	nc, err := nats.Connect(m.Cfg.NatsUrl, opts...)
	if err != nil {
		msg := fmt.Errorf("Can't connect to "+
			"nats on url '%s': %v",
			m.Cfg.NatsUrl,
			err)
		m.Cfg.Log.Errorf(msg.Error())
		return msg
	}
	m.nc = nc
	loc, err := m.getNatsServerLocation()
	if err != nil {
		return err
	}
	m.setLoc(loc)
	m.Cfg.Log.Debugf("health-agent: HELLOWORLD: "+
		"I am '%s' at '%v:%v'. "+
		"rank %v",
		m.myLoc.Id,
		m.myLoc.Host,
		m.myLoc.Port,
		m.myLoc.Rank)

	m.subjAllCall = sysMemberPrefix + "allcall"
	m.subjAllReply = sysMemberPrefix + "allreply"
	m.subjMemberLost = sysMemberPrefix + "lost"
	m.subjMemberAdded = sysMemberPrefix + "added"
	m.subjMembership = sysMemberPrefix + "list"

	nc.Subscribe(m.subjAllReply, func(msg *nats.Msg) {
		if m.deaf() {
			return
		}
		pc.receivePong(msg)
	})

	// allcall says: "here's my leader, who is out there?"
	nc.Subscribe(m.subjAllCall, func(msg *nats.Msg) {
		if m.deaf() {
			return
		}
		loc, err := m.getNatsServerLocation()
		if err != nil {
			return // try again next time.
		}

		// did we accidentally change
		// server locacations?
		// Yikes, we don't want to do that!
		// We are supposed to be monitoring
		// just our own server.
		if m.locDifferent(loc) {
			panic(fmt.Sprintf("\n very bad! health-agent "+
				"changed locations! "+
				"first: '%s',\n\nvs\n now:'%s'\n",
				&m.myLoc,
				loc))
		}

		/*
			// allcall broadcasts the sender's leader
			var lead ServerLoc
			err = lead.fromBytes(msg.Data)
			panicOn(err)

			if lead.Id != "" && !lead.LeaseExpires.IsZero() {
				won, alt := m.elec.setLeader(&lead)
				if !won {
					//p("port %v, at 111 in allcall handler: !won: rejected '%s' in favor of alt '%s'", m.myLoc.Port, &lead, &alt)
					// if we rejected, get our preferred leader.
					lead = alt
				}

				if loc.Id == lead.Id {
					loc.IsLeader = true
					loc.LeaseExpires = lead.LeaseExpires
				} else {
					loc.IsLeader = false
					loc.LeaseExpires = time.Time{}
				}
			}
		*/
		hp, err := json.Marshal(loc)
		panicOn(err)
		if !m.deaf() {
			pong(nc, msg.Reply, hp)
		}
	})

	// reporting
	nc.Subscribe(m.subjMemberLost, func(msg *nats.Msg) {
		if m.deaf() {
			return
		}
		m.Cfg.Log.Tracef("health-agent: "+
			"Received on [%s]: '%s'",
			msg.Subject,
			string(msg.Data))
	})

	// reporting
	nc.Subscribe(m.subjMemberAdded, func(msg *nats.Msg) {
		if m.deaf() {
			return
		}
		m.Cfg.Log.Tracef("health-agent: Received on [%s]: '%s'",
			msg.Subject, string(msg.Data))
	})

	// reporting
	nc.Subscribe(m.subjMembership, func(msg *nats.Msg) {
		if m.deaf() {
			return
		}
		m.Cfg.Log.Tracef("health-agent: "+
			"Received on [%s]: '%s'",
			msg.Subject,
			string(msg.Data))
	})

	return nil
}

func (m *Membership) locDifferent(b *nats.ServerLoc) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.Id != m.myLoc.Id {
		return true
	}
	if b.Rank != m.myLoc.Rank {
		return true
	}
	if b.Host != m.myLoc.Host {
		return true
	}
	if b.Port != m.myLoc.Port {
		return true
	}
	return false
}

func (m *Membership) setLoc(b *nats.ServerLoc) {
	m.mu.Lock()
	m.myLoc.Id = b.Id
	m.myLoc.Rank = b.Rank
	m.myLoc.Host = b.Host
	m.myLoc.Port = b.Port
	m.mu.Unlock()
	m.elec.setMyLoc(&m.myLoc)
}

func (m *Membership) getNatsServerLocation() (*nats.ServerLoc, error) {
	loc, err := m.nc.ServerLocation()
	if err != nil {
		return nil, err
	}
	// fill in the rank because server
	// doesn't have the rank correct under
	// various test scenarios where we
	// spin up an embedded gnatsd.
	//
	// This is still correct in non-test,
	// since the health-agent will
	// have read from the command line
	// -rank options and then
	// configured Cfg.MyRank when running
	// embedded as an internal client.
	loc.Rank = m.Cfg.MyRank
	return loc, nil
}
