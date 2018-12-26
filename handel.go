package handel

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type Level struct {
	id int
	nodes []Identity
	started bool
	completed bool
	finished bool
	pos int
	sent int
	currentBestSize int
}

func NewLevel(id int, nodes []Identity) *Level {
	if id <= 0 {
		panic("bad value for level id")
	}
	l := &Level{
		id,
		nodes,
		true,
		false, // For the first level, we need only our own sig
		false,
		0,
		0,
		0,
	}
	return l
}

func createLevels(r Registry, partitioner Partitioner) []Level{
	lvls := make( []Level, log2(r.Size()))

	for i := 0; i< len(lvls); i += 1 {
		nodes, _ := partitioner.PickNextAt(i+1, r.Size() + 1)
		lvls[i] = *NewLevel(i+1, nodes)
	}

	return lvls
}


func (c *Level) PickNextAt(count int) ([]Identity, bool) {
	size := min(count, len(c.nodes))
	res := make( []Identity, size)

	for i:=0; i<size; i++{
		res[i] = c.nodes[c.pos]
		c.pos++
		if c.pos >= len(c.nodes){
			c.pos = 0
		}
	}

	c.sent += size
	if c.sent >= len(c.nodes) {
		c.finished = true
	}

	return res, true
}

func (l *Level) updateBestSig(sig *MultiSignature) (bool) {
	if sig.BitSet.Cardinality() > len(l.nodes) {
		msg := fmt.Sprintf ("Too many signatures for this level: lvl=%d, nodes=%d, sigs=%d",
			l.id, len(l.nodes), sig.BitSet.Cardinality())
		panic(msg)
	}
	if l.currentBestSize >= sig.BitSet.Cardinality() {
		return false
	}

	// We update our best sig. It means has well that
	//  we will reset our counter of sent messages
	l.currentBestSize = sig.Cardinality()
	l.finished = false
	l.sent = 0

	return l.currentBestSize == len(l.nodes)
}

func (h *Handel) sendUpdate(l Level, count int) {
	if !l.started || l.finished {
		return
	}

	sp := h.store.Combined(byte(l.id) - 1)
	if sp == nil {
		panic("THIS SHOULD NOT HAPPEN AT ALL")
	}
	newNodes, _ := l.PickNextAt(count)
	h.logf("sending out signature of lvl %d (size %d) to %v", l.id, sp.BitSet.BitLength(), newNodes)
	h.sendTo(l.id, sp, newNodes)
}

// Handel is the principal struct that performs the large scale multi-signature
// aggregation protocol. Handel is thread-safe.
type Handel struct {
	sync.Mutex
	// Config holding parameters to Handel
	c *Config
	// Network enabling external communication with other Handel nodes
	net Network
	// Registry holding access to all Handel node's identities
	reg Registry
	// constructor to unmarshal signatures + aggregate pub keys
	cons Constructor
	// public identity of this Handel node
	id Identity
	// Message that is being signed during the Handel protocol
	msg []byte
	// signature over the message
	sig Signature
	// signature store with different merging/caching strategy
	store signatureStore
	// processing of signature - verification strategy
	proc signatureProcessing
	// all actors registered that acts on a new signature
	actors []actor
	// highest level attained by this handel node so far
	currLevel byte
	// best final signature,i.e. at the last level, seen so far
	best *MultiSignature
	// channel to exposes multi-signatures to the user
	out chan MultiSignature
	// indicating whether handel is finished or not
	done bool
	// constant threshold of contributions required in a ms to be considered
	// valid
	threshold int
	// ticker for the periodic update
	ticker *time.Ticker
	// all the levels
	levels []Level
}


// NewHandel returns a Handle interface that uses the given network and
// registry. The identity is the public identity of this Handel's node. The
// constructor defines over which curves / signature scheme Handel runs. The
// message is the message to "multi-sign" by Handel.  The first config in the
// slice is taken if not nil. Otherwise, the default config generated by
// DefaultConfig() is used.
func NewHandel(n Network, r Registry, id Identity, c Constructor,
	msg []byte, s Signature, conf ...*Config) *Handel {

	var config *Config
	if len(conf) > 0 && conf[0] != nil {
		config = mergeWithDefault(conf[0], r.Size())
	} else {
		config = DefaultConfig(r.Size())
	}

	part := config.NewPartitioner(id.ID(), r)
	firstBs := config.NewBitSet(1)
	firstBs.Set(0, true)
	mySig := &MultiSignature{BitSet: firstBs, Signature: s}

	h := &Handel{
		c:        config,
		net:      n,
		reg:      r,
		id:       id,
		cons:     c,
		msg:      msg,
		sig:      s,
		out:      make(chan MultiSignature, 100),
		ticker:	  time.NewTicker(config.UpdatePeriod),
		levels:   createLevels(r, part),
	}
	h.actors = []actor{
		actorFunc(h.checkCompletedLevel),
		actorFunc(h.checkFinalSignature),
	}

	go func() {
		for t := range h.ticker.C {
			if false {
				print(t)
			}
			h.periodicUpdate()
		}
	}()

	h.threshold = h.c.ContributionsThreshold(h.reg.Size())
	h.store = newReplaceStore(part, h.c.NewBitSet)
	h.store.Store(0, mySig)
	h.proc = newFifoProcessing(h.store, part, c, msg)
	h.net.RegisterListener(h)
	return h
}

// NewPacket implements the Listener interface for the network.
// it parses the packet and sends it to processing if the packet is properly
// formatted.
func (h *Handel) NewPacket(p *Packet) {
	h.Lock()
	defer h.Unlock()
	if h.done {
		return
	}
	ms, err := h.parsePacket(p)
	if err != nil {
		h.logf("invalid packet: %s", err)
		return
	}

	// sends it to processing
	h.logf("received packet from %d for level %d: %s", p.Origin, p.Level, ms.String())
	h.proc.Incoming() <- sigPair{origin: p.Origin, level: p.Level, ms: ms}
}

// Start the Handel protocol by sending signatures to peers in the first level,
// and by starting relevant sub routines.
func (h *Handel) Start() {
	h.Lock()
	defer h.Unlock()
	go h.proc.Start()
	go h.rangeOnVerified()
	h.startNextLevel()
}

// Stop the Handel protocol and all sub routines
func (h *Handel) Stop() {
	h.Lock()
	defer h.Unlock()
	h.ticker.Stop()
	h.proc.Stop()
	h.done = true
	close(h.out)
}

func (h *Handel) periodicUpdate() {
	h.Lock()
	defer h.Unlock()
	for _, lvl := range h.levels {
		h.sendUpdate(lvl, 1)
	}
}

// FinalSignatures returns the channel over which final multi-signatures
// are sent over. These multi-signatures contain at least a threshold of
// contributions, as defined in the config.
func (h *Handel) FinalSignatures() chan MultiSignature {
	return h.out
}

// startNextLevel increase the currLevel counter and sends its best
// highest-level signature it has to nodes at the new currLevel.
// method is NOT thread-safe.
func (h *Handel) startNextLevel() {
	if int(h.currLevel) >= len(h.levels) {
		// protocol is finished
		h.logf("protocol finished at level %d", h.currLevel)
		return
	}
	//h.findNextLevel()
	h.sendBestUpTo(int(h.currLevel))
	// increase the max level we are at
	h.currLevel++
	h.logf("Passing to a new level %d -> %d", h.currLevel-1, h.currLevel)
}

// rangeOnVerified continuously listens on the output channel of the signature
// processing routine for verified signatures. Each verified signatures is
// passed down to all registered actors. Each handler is called in a thread safe
// manner, global lock is held during the call to actors.
func (h *Handel) rangeOnVerified() {
	for v := range h.proc.Verified() {
		h.logf("new verified signature received -> %s", v.String())
		h.store.Store(v.level, v.ms)
		h.Lock()
		for _, actor := range h.actors {
			actor.OnVerifiedSignature(&v)
		}
		h.Unlock()
	}
}

// actor is an interface that takes a new verified signature and acts on it
// according to its own rule. It can be checking if it passes to a next level,
// checking if the protocol is finished, checking if a signature completes
// higher levels so it should send it out to other peers, etc. The store is
// guaranteed to have a multisignature present at the level indicated in the
// verifiedSig. Each handler is called in a thread safe manner, global lock is
// held during the call to actors.
type actor interface {
	OnVerifiedSignature(s *sigPair)
}

type actorFunc func(s *sigPair)

func (a actorFunc) OnVerifiedSignature(s *sigPair) {
	a(s)
}

// checkFinalSignature STORES the newly verified signature and then checks if a
// new better final signature, i.e. a signature at the last level, has been
// generated. If so, it sends it to the output channel.
func (h *Handel) checkFinalSignature(s *sigPair) {
	sig := h.store.FullSignature()

	if sig.BitSet.Cardinality() < h.threshold {
		return
	}
	newBest := func(ms *MultiSignature) {
		if h.done {
			return
		}
		h.best = ms
		h.out <- *h.best
	}

	if h.best == nil {
		newBest(sig)
		return
	}

	newCard := sig.Cardinality()
	local := h.best.Cardinality()
	if newCard > local {
		newBest(sig)
	}
}

// checkCompletedLevel looks if the signature completes its respective level. If it
// does, handel sends it out to new peers for this level if possible.
func (h *Handel) checkCompletedLevel(s *sigPair) {
	lvl := h.levels[s.level-1]
	if lvl.completed {
		return // fast exit
	}

	// XXX IIF completed signatures for higher level then send this higher level
	// instead
	ms, ok := h.store.Best(s.level)
	if !ok {
		panic("something's wrong with the store")
	}
	if !lvl.updateBestSig(ms) {
		return
	}

	// go to next level if we already finished this one !
	// XXX: this should be moved to a handler "checkGoToNextLevel" that checks
	// if the combined signature has enough cardinality to pass to higher levels
	if s.level == h.currLevel {
		h.startNextLevel()
		return
	}

	// Now we check from 1st level to this level if we have them all completed.
	// if it is the case, then we create the combined signature of all these
	// levels, and send that up to the next. This part is redundant only if we
	// start the new level (that's the same action being done), but we might be
	// already at a higher level with incomplete signature so this is where it's
	// important: to improve over existing levels.
	if  lvl.id < len(h.levels) - 1 {
		h.sendBestUpTo(lvl.id)
	}
}

// sendBestUpTo computes the best signature possible at the given level, and
// sends it out to new nodes at level at least level + 1. It may send it to
// nodes at highest level if the intermediate levels are empty (it happens if n
// is not a power of two).  This call may not send signatures if the level given
// is already at the maximum level so it's not possible to send a `Combined`
// signature anymore - this handel node can fetch its full signature already.
// lvl can be equals to zero!
func (h *Handel) sendBestUpTo(lvl int) {
	if lvl < 0 || lvl >= len(h.levels) {
		msg := fmt.Sprintf ("skip sending best -> reached maximum level %d/%d", lvl, len(h.levels))
		panic(msg)
	}

	levelToSend, err := h.findNextLevel(lvl)
	if err != nil {
		panic(err)
	}

	h.sendUpdate(h.levels[levelToSend-1], h.c.CandidateCount)
}

// findNextLevel loops from lvl+1 to max level to find a level which is not
// empty and returns that level
func (h *Handel) findNextLevel(lvl int) (int, error) {
	for l := lvl + 1; lvl <= len(h.levels); l++ {
		fullSize := len(h.levels[l-1].nodes)
		if fullSize == 0 {
			continue
		}
		return l, nil
	}
	return 0, errors.New("no non-empty level found")
}

func (h *Handel) sendTo(lvl int, ms *MultiSignature, ids []Identity) {
	buff, err := ms.MarshalBinary()
	if err != nil {
		h.logf("error marshalling multi-signature: %s", err)
		return
	}

	packet := &Packet{
		Origin:   h.id.ID(),
		Level:    byte(lvl),
		MultiSig: buff,
	}
	h.net.Send(ids, packet)
}

// parsePacket returns the multisignature parsed from the given packet, or an
// error if the packet can't be unmarshalled, or contains erroneous data such as
// out of range level.  This method is NOT thread-safe and only meant for
// internal use.
func (h *Handel) parsePacket(p *Packet) (*MultiSignature, error) {
	if p.Origin >= int32(h.reg.Size()) {
		return nil, errors.New("packet's origin out of range")
	}

	lvl := int(p.Level)
	if lvl  < 1 || lvl > log2(h.reg.Size()) {
		msg := fmt.Sprintf("packet's level out of range, level received=%d, max=%d, nodes count=%d",
			lvl, log2(h.reg.Size()), h.reg.Size())
		return nil, errors.New(msg)
	}

	ms := new(MultiSignature)
	err := ms.Unmarshal(p.MultiSig, h.cons.Signature(), h.c.NewBitSet)
	return ms, err
}

func (h *Handel) logf(str string, args ...interface{}) {
	now := time.Now()
	timeSpent := fmt.Sprintf("%02d:%02d:%02d", now.Hour(),
		now.Minute(),
		now.Second())
	idArg := []interface{}{timeSpent, h.id.ID()}
	logf("%s: handel %d: "+str, append(idArg, args...)...)
}
