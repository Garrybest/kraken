package dispatch

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"code.uber.internal/infra/kraken/.gen/go/p2p"
	"code.uber.internal/infra/kraken/core"
	"code.uber.internal/infra/kraken/lib/torrent/networkevent"
	"code.uber.internal/infra/kraken/lib/torrent/scheduler/conn"
	"code.uber.internal/infra/kraken/lib/torrent/scheduler/dispatch/piecerequest"
	"code.uber.internal/infra/kraken/lib/torrent/storage"
	"code.uber.internal/infra/kraken/utils/log"

	"github.com/andres-erbsen/clock"
	"github.com/uber-go/tally"
	"github.com/willf/bitset"
	"go.uber.org/zap"
	"golang.org/x/sync/syncmap"
)

var (
	errPeerAlreadyDispatched   = errors.New("peer is already dispatched for the torrent")
	errPieceOutOfBounds        = errors.New("piece index out of bounds")
	errChunkNotSupported       = errors.New("reading / writing chunk of piece not supported")
	errRepeatedBitfieldMessage = errors.New("received repeated bitfield message")
)

// Events defines Dispatcher events.
type Events interface {
	DispatcherComplete(*Dispatcher)
}

// Messages defines a subset of conn.Conn methods which Dispatcher requires to
// communicate with remote peers.
type Messages interface {
	Send(msg *conn.Message) error
	Receiver() <-chan *conn.Message
	Close()
}

// Dispatcher coordinates torrent state with sending / receiving messages between multiple
// peers. As such, Dispatcher and Torrent have a one-to-one relationship, while Dispatcher
// and Conn have a one-to-many relationship.
type Dispatcher struct {
	config                Config
	stats                 tally.Scope
	clk                   clock.Clock
	createdAt             time.Time
	localPeerID           core.PeerID
	torrent               *torrentAccessWatcher
	peers                 syncmap.Map // Maps core.PeerID to *peer.
	netevents             networkevent.Producer
	pieceRequestTimeout   time.Duration
	pieceRequestManager   *piecerequest.Manager
	pendingPiecesDoneOnce sync.Once
	pendingPiecesDone     chan struct{}
	completeOnce          sync.Once
	events                Events
}

// New creates a new Dispatcher.
func New(
	config Config,
	stats tally.Scope,
	clk clock.Clock,
	netevents networkevent.Producer,
	events Events,
	peerID core.PeerID,
	t storage.Torrent) *Dispatcher {

	d := newDispatcher(config, stats, clk, netevents, events, peerID, t)

	// Exits when d.pendingPiecesDone is closed.
	go d.watchPendingPieceRequests()

	if t.Complete() {
		d.complete()
	}

	return d
}

// newDispatcher creates a new Dispatcher with no side-effects for testing purposes.
func newDispatcher(
	config Config,
	stats tally.Scope,
	clk clock.Clock,
	netevents networkevent.Producer,
	events Events,
	peerID core.PeerID,
	t storage.Torrent) *Dispatcher {

	config = config.applyDefaults()

	stats = stats.Tagged(map[string]string{
		"module": "dispatch",
	})

	pieceRequestTimeout := config.calcPieceRequestTimeout(t.MaxPieceLength())
	pieceRequestManager := piecerequest.NewManager(clk, pieceRequestTimeout, config.PipelineLimit)

	return &Dispatcher{
		config:              config,
		stats:               stats,
		clk:                 clk,
		createdAt:           clk.Now(),
		localPeerID:         peerID,
		torrent:             newTorrentAccessWatcher(t, clk),
		netevents:           netevents,
		pieceRequestTimeout: pieceRequestTimeout,
		pieceRequestManager: pieceRequestManager,
		pendingPiecesDone:   make(chan struct{}),
		events:              events,
	}
}

// Name returns d's torrent name.
func (d *Dispatcher) Name() string {
	return d.torrent.Name()
}

// InfoHash returns d's torrent hash.
func (d *Dispatcher) InfoHash() core.InfoHash {
	return d.torrent.InfoHash()
}

// Length returns d's torrent length.
func (d *Dispatcher) Length() int64 {
	return d.torrent.Length()
}

// Stat returns d's TorrentInfo.
func (d *Dispatcher) Stat() *storage.TorrentInfo {
	return d.torrent.Stat()
}

// Complete returns true if d's torrent is complete.
func (d *Dispatcher) Complete() bool {
	return d.torrent.Complete()
}

// CreatedAt returns when d was created.
func (d *Dispatcher) CreatedAt() time.Time {
	return d.createdAt
}

// LastGoodPieceReceived returns when d last received a valid and needed piece
// from peerID.
func (d *Dispatcher) LastGoodPieceReceived(peerID core.PeerID) time.Time {
	v, ok := d.peers.Load(peerID)
	if !ok {
		return time.Time{}
	}
	return v.(*peer).getLastGoodPieceReceived()
}

// LastPieceSent returns when d last sent a piece to peerID.
func (d *Dispatcher) LastPieceSent(peerID core.PeerID) time.Time {
	v, ok := d.peers.Load(peerID)
	if !ok {
		return time.Time{}
	}
	return v.(*peer).getLastPieceSent()
}

// LastReadTime returns when d's torrent was last read from.
func (d *Dispatcher) LastReadTime() time.Time {
	return d.torrent.getLastReadTime()
}

// LastWriteTime returns when d's torrent was last written to.
func (d *Dispatcher) LastWriteTime() time.Time {
	return d.torrent.getLastWriteTime()
}

// Empty returns true if the Dispatcher has no peers.
func (d *Dispatcher) Empty() bool {
	// syncmap.Map does not provide a length function, hence this poor man's
	// implementation of `len(d.peers) == 0`.
	empty := true
	d.peers.Range(func(k, v interface{}) bool {
		empty = false
		return false
	})
	return empty
}

// AddPeer registers a new peer with the Dispatcher.
func (d *Dispatcher) AddPeer(
	peerID core.PeerID, b *bitset.BitSet, messages Messages) error {

	p, err := d.addPeer(peerID, b, messages)
	if err != nil {
		return err
	}
	go d.maybeRequestMorePieces(p)
	go d.feed(p)
	return nil
}

// addPeer creates and inserts a new peer into the Dispatcher. Split from AddPeer
// with no goroutine side-effects for testing purposes.
func (d *Dispatcher) addPeer(
	peerID core.PeerID, b *bitset.BitSet, messages Messages) (*peer, error) {

	p := newPeer(peerID, b, messages, d.clk)
	if _, ok := d.peers.LoadOrStore(peerID, p); ok {
		return nil, errors.New("peer already exists")
	}
	return p, nil
}

// TearDown closes all Dispatcher connections.
func (d *Dispatcher) TearDown() {
	d.pendingPiecesDoneOnce.Do(func() {
		close(d.pendingPiecesDone)
	})
	d.peers.Range(func(k, v interface{}) bool {
		p := v.(*peer)
		d.log("peer", p).Info("Dispatcher teardown closing connection")
		p.messages.Close()
		return true
	})
}

func (d *Dispatcher) String() string {
	return fmt.Sprintf("Dispatcher(%s)", d.torrent)
}

func (d *Dispatcher) complete() {
	d.completeOnce.Do(func() { go d.events.DispatcherComplete(d) })
	d.pendingPiecesDoneOnce.Do(func() { close(d.pendingPiecesDone) })

	d.peers.Range(func(k, v interface{}) bool {
		p := v.(*peer)
		if p.bitfield.Complete() {
			// Close connections to other completed peers since those connections
			// are now useless.
			d.log("peer", p).Info("Closing connection to completed peer")
			p.messages.Close()
		} else {
			// Notify in-progress peers that we have completed the torrent and
			// all pieces are available.
			p.messages.Send(conn.NewCompleteMessage())
		}
		return true
	})
}

func (d *Dispatcher) endgame() bool {
	if d.config.DisableEndgame {
		return false
	}
	remaining := d.torrent.NumPieces() - int(d.torrent.Bitfield().Count())
	return remaining <= d.config.EndgameThreshold
}

func (d *Dispatcher) maybeRequestMorePieces(p *peer) (bool, error) {
	candidates := p.bitfield.Intersection(d.torrent.Bitfield().Complement())
	return d.maybeSendPieceRequests(p, candidates)
}

func (d *Dispatcher) maybeSendPieceRequests(p *peer, candidates *bitset.BitSet) (bool, error) {
	pieces := d.pieceRequestManager.ReservePieces(p.id, candidates, d.endgame())
	if len(pieces) == 0 {
		return false, nil
	}
	for _, i := range pieces {
		if err := p.messages.Send(conn.NewPieceRequestMessage(i, d.torrent.PieceLength(i))); err != nil {
			// Connection closed.
			d.pieceRequestManager.MarkUnsent(p.id, i)
			return false, err
		}
	}
	return true, nil
}

func (d *Dispatcher) resendFailedPieceRequests() {
	failedRequests := d.pieceRequestManager.GetFailedRequests()
	if len(failedRequests) > 0 {
		d.log().Infof("Resending %d failed piece requests", len(failedRequests))
		d.stats.Counter("piece_request_failures").Inc(int64(len(failedRequests)))
	}

	var sent int
	for _, r := range failedRequests {
		d.peers.Range(func(k, v interface{}) bool {
			p := v.(*peer)
			if (r.Status == piecerequest.StatusExpired || r.Status == piecerequest.StatusInvalid) &&
				r.PeerID == p.id {
				// Do not resend to the same peer for expired or invalid requests.
				return true
			}

			b := d.torrent.Bitfield()
			candidates := p.bitfield.Intersection(b.Complement())
			if candidates.Test(uint(r.Piece)) {
				nb := bitset.New(b.Len()).Set(uint(r.Piece))
				if sent, err := d.maybeSendPieceRequests(p, nb); sent && err == nil {
					return false
				}
			}
			return true
		})
	}

	unsent := len(failedRequests) - sent
	if unsent > 0 {
		d.log().Infof("Nowhere to resend %d / %d failed piece requests", unsent, len(failedRequests))
	}
}

func (d *Dispatcher) watchPendingPieceRequests() {
	for {
		select {
		case <-d.clk.After(d.pieceRequestTimeout / 2):
			d.resendFailedPieceRequests()
		case <-d.pendingPiecesDone:
			return
		}
	}
}

// feed reads off of peer and handles incoming messages. When peer's messages close,
// the feed goroutine removes peer from the Dispatcher and exits.
func (d *Dispatcher) feed(p *peer) {
	for msg := range p.messages.Receiver() {
		if err := d.dispatch(p, msg); err != nil {
			d.log().Errorf("Error dispatching message: %s", err)
		}
	}
	d.peers.Delete(p.id)
	d.pieceRequestManager.ClearPeer(p.id)
}

func (d *Dispatcher) dispatch(p *peer, msg *conn.Message) error {
	switch msg.Message.Type {
	case p2p.Message_ERROR:
		d.handleError(p, msg.Message.Error)
	case p2p.Message_ANNOUCE_PIECE:
		d.handleAnnouncePiece(p, msg.Message.AnnouncePiece)
	case p2p.Message_PIECE_REQUEST:
		d.handlePieceRequest(p, msg.Message.PieceRequest)
	case p2p.Message_PIECE_PAYLOAD:
		d.handlePiecePayload(p, msg.Message.PiecePayload, msg.Payload)
	case p2p.Message_CANCEL_PIECE:
		d.handleCancelPiece(p, msg.Message.CancelPiece)
	case p2p.Message_BITFIELD:
		d.handleBitfield(p, msg.Message.Bitfield)
	case p2p.Message_COMPLETE:
		d.handleComplete(p)
	default:
		return fmt.Errorf("unknown message type: %d", msg.Message.Type)
	}
	return nil
}

func (d *Dispatcher) handleError(p *peer, msg *p2p.ErrorMessage) {
	switch msg.Code {
	case p2p.ErrorMessage_PIECE_REQUEST_FAILED:
		d.log().Errorf("Piece request failed: %s", msg.Error)
		d.pieceRequestManager.MarkInvalid(p.id, int(msg.Index))
	}
}

func (d *Dispatcher) handleAnnouncePiece(p *peer, msg *p2p.AnnouncePieceMessage) {
	if int(msg.Index) >= d.torrent.NumPieces() {
		d.log().Errorf("Announce piece out of bounds: %d >= %d", msg.Index, d.torrent.NumPieces())
		return
	}
	i := int(msg.Index)
	p.bitfield.Set(uint(i), true)

	d.maybeRequestMorePieces(p)
}

func (d *Dispatcher) isFullPiece(i, offset, length int) bool {
	return offset == 0 && length == int(d.torrent.PieceLength(i))
}

func (d *Dispatcher) handlePieceRequest(p *peer, msg *p2p.PieceRequestMessage) {
	i := int(msg.Index)
	if !d.isFullPiece(i, int(msg.Offset), int(msg.Length)) {
		d.log("peer", p, "piece", i).Error("Rejecting piece request: chunk not supported")
		p.messages.Send(conn.NewErrorMessage(i, p2p.ErrorMessage_PIECE_REQUEST_FAILED, errChunkNotSupported))
		return
	}

	payload, err := d.torrent.GetPieceReader(i)
	if err != nil {
		d.log("peer", p, "piece", i).Errorf("Error getting reader for requested piece: %s", err)
		p.messages.Send(conn.NewErrorMessage(i, p2p.ErrorMessage_PIECE_REQUEST_FAILED, err))
		return
	}

	if err := p.messages.Send(conn.NewPiecePayloadMessage(i, payload)); err != nil {
		return
	}

	p.touchLastPieceSent()

	// Assume that the peer successfully received the piece.
	p.bitfield.Set(uint(i), true)
}

func (d *Dispatcher) handlePiecePayload(
	p *peer, msg *p2p.PiecePayloadMessage, payload storage.PieceReader) {

	defer payload.Close()

	i := int(msg.Index)
	if !d.isFullPiece(i, int(msg.Offset), int(msg.Length)) {
		d.log("peer", p, "piece", i).Error("Rejecting piece payload: chunk not supported")
		d.pieceRequestManager.MarkInvalid(p.id, i)
		return
	}

	if err := d.torrent.WritePiece(payload, i); err != nil {
		if err != storage.ErrPieceComplete {
			d.log("peer", p, "piece", i).Errorf("Error writing piece payload: %s", err)
			d.pieceRequestManager.MarkInvalid(p.id, i)
		}
		return
	}

	d.netevents.Produce(
		networkevent.ReceivePieceEvent(d.torrent.InfoHash(), d.localPeerID, p.id, i))

	p.touchLastGoodPieceReceived()
	if d.torrent.Complete() {
		d.complete()
	}

	d.pieceRequestManager.Clear(i)

	d.maybeRequestMorePieces(p)

	d.peers.Range(func(k, v interface{}) bool {
		if k.(core.PeerID) == p.id {
			return true
		}
		pp := v.(*peer)

		pp.messages.Send(conn.NewAnnouncePieceMessage(i))

		return true
	})
}

func (d *Dispatcher) handleCancelPiece(p *peer, msg *p2p.CancelPieceMessage) {
	// No-op: cancelling not supported because all received messages are synchronized,
	// therefore if we receive a cancel it is already too late -- we've already read
	// the piece.
}

func (d *Dispatcher) handleBitfield(p *peer, msg *p2p.BitfieldMessage) {
	d.log("peer", p).Error("Unexpected bitfield message from established conn")
}

func (d *Dispatcher) handleComplete(p *peer) {
	if d.Complete() {
		d.log("peer", p).Info("Closing connection to completed peer")
		p.messages.Close()
	} else {
		p.bitfield.SetAll(true)
		d.maybeRequestMorePieces(p)
	}
}

func (d *Dispatcher) log(args ...interface{}) *zap.SugaredLogger {
	args = append(args, "torrent", d.torrent)
	return log.With(args...)
}
