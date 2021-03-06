package rpc

import (
	"context"
	"fnd/blob"
	"fnd/crypto"
	"fnd/log"
	"fnd/p2p"
	apiv1 "fnd/rpc/v1"
	"fnd/store"
	"fnd/util"
	"fnd/wire"
	"github.com/pkg/errors"
	"github.com/syndtr/goleveldb/leveldb"
	"google.golang.org/grpc"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	TransactionExpiry = 15000
)

var emptyRes = &apiv1.Empty{}

type Opts struct {
	PeerID      crypto.Hash
	BlobStore   blob.Store
	PeerManager p2p.PeerManager
	NameLocker  util.MultiLocker
	Mux         *p2p.PeerMuxer
	DB          *leveldb.DB
	Host        string
	Port        int
}

type Server struct {
	peerID     crypto.Hash
	host       string
	port       int
	mux        *p2p.PeerMuxer
	db         *leveldb.DB
	bs         blob.Store
	pm         p2p.PeerManager
	nameLocker util.MultiLocker
	txStore    *util.Cache
	lgr        log.Logger
	lastTxID   uint32
	srv        *grpc.Server
}

type awaitingTx struct {
	blob blob.Blob
	tx   blob.Transaction
}

func NewServer(opts *Opts) *Server {
	lgr := log.WithModule("rpc-server")

	srv := &Server{
		peerID:     opts.PeerID,
		host:       opts.Host,
		port:       opts.Port,
		mux:        opts.Mux,
		db:         opts.DB,
		bs:         opts.BlobStore,
		pm:         opts.PeerManager,
		nameLocker: opts.NameLocker,
		txStore:    util.NewCache(),
		lgr:        lgr,
	}
	srv.txStore.ReaperFunc = func(pub string, val interface{}) {
		awaiting := val.(*awaitingTx)
		err := awaiting.tx.Rollback()
		if err == nil {
			lgr.Info("reaped stale blob transaction", "pub", pub)
		} else {
			lgr.Error("failed to remove stale blob transaction", "err", err, "pub", pub)
		}
		if err := awaiting.blob.Close(); err != nil {
			lgr.Error("error closing blob", "err", err)
		}
	}
	return srv
}

func (s *Server) Start() error {
	lis, err := net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return err
	}
	s.srv = grpc.NewServer()
	apiv1.RegisterFootnotev1Server(s.srv, s)
	go s.srv.Serve(lis)
	return nil
}

func (s *Server) Stop() error {
	s.srv.Stop()
	return nil
}

func (s *Server) GetStatus(context.Context, *apiv1.Empty) (*apiv1.GetStatusRes, error) {
	in, out := s.mux.PeerCount()
	peerCount := in + out
	tx, rx := s.mux.BandwidthUsage()
	hc, err := store.GetHeaderCount(s.db)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetStatusRes{
		PeerID:      s.peerID[:],
		PeerCount:   uint32(peerCount),
		HeaderCount: uint32(hc),
		TxBytes:     tx,
		RxBytes:     rx,
	}, nil
}

func (s *Server) AddPeer(_ context.Context, req *apiv1.AddPeerReq) (*apiv1.Empty, error) {
	if req.VerifyPeerID && len(req.PeerID) != 32 {
		return nil, errors.New("peer id must be 32 bytes")
	}
	var peerId crypto.Hash
	copy(peerId[:], req.PeerID)
	if err := s.pm.DialPeer(peerId, req.Ip, req.VerifyPeerID); err != nil {
		return nil, err
	}
	return emptyRes, nil
}

func (s *Server) BanPeer(_ context.Context, req *apiv1.BanPeerReq) (*apiv1.Empty, error) {
	ip := net.ParseIP(req.Ip).To4()
	if ip == nil {
		return nil, errors.New("invalid IP")
	}

	dur := time.Duration(req.DurationMS) * time.Millisecond
	err := store.WithTx(s.db, func(tx *leveldb.Transaction) error {
		if err := store.BanInboundPeerTx(tx, ip.String(), dur); err != nil {
			return err
		}
		return store.BanOutboundPeerTx(tx, ip.String(), dur)
	})
	if err != nil {
		return nil, errors.Wrap(err, "error storing peer data")
	}

	peers := s.mux.PeersByIP(ip.String())
	for _, peer := range peers {
		if err := peer.Close(); err != nil {
			return nil, errors.Wrap(err, "error closing peer")
		}
	}

	return emptyRes, nil
}

func (s *Server) UnbanPeer(_ context.Context, req *apiv1.UnbanPeerReq) (*apiv1.Empty, error) {
	ip := net.ParseIP(req.Ip).To4()
	if ip == nil {
		return emptyRes, errors.New("invalid IP")
	}
	err := store.WithTx(s.db, func(tx *leveldb.Transaction) error {
		if err := store.UnbanInboundPeerTx(tx, ip.String()); err != nil {
			return err
		}
		return store.UnbanOutboundPeerTx(tx, ip.String())
	})
	if err != nil {
		return nil, errors.Wrap(err, "error storing peer data")
	}
	return emptyRes, nil
}

func (s *Server) ListPeers(req *apiv1.ListPeersReq, stream apiv1.Footnotev1_ListPeersServer) error {
	connectedPeers := s.mux.Peers()
	storedPeers, err := store.StreamPeers(s.db, true)
	if err != nil {
		return errors.Wrap(err, "error opening peer stream")
	}

	for {
		peer, err := storedPeers.Next()
		if err != nil {
			return errors.Wrap(err, "error streaming peer data")
		}
		if peer == nil {
			return nil
		}

		var txBytes uint64
		var rxBytes uint64
		var connected bool
		livePeer := connectedPeers[peer.ID]
		if livePeer != nil {
			txBytes, rxBytes = livePeer.BandwidthUsage()
			connected = true
		}

		peerRes := &apiv1.ListPeersRes{
			PeerID:      peer.ID[:],
			Ip:          peer.IP,
			Banned:      peer.IsBanned(),
			Whitelisted: peer.Whitelisted,
			Connected:   connected,
			TxBytes:     txBytes,
			RxBytes:     rxBytes,
		}
		if err := stream.Send(peerRes); err != nil {
			return err
		}
	}
}

func (s *Server) Checkout(ctx context.Context, req *apiv1.CheckoutReq) (*apiv1.CheckoutRes, error) {
	txID := atomic.AddUint32(&s.lastTxID, 1)
	bl, err := s.bs.Open(req.Name)
	if err != nil {
		return nil, err
	}
	tx, err := bl.Transaction()
	if err != nil {
		return nil, err
	}

	s.txStore.Set(strconv.FormatUint(uint64(txID), 32), &awaitingTx{
		blob: bl,
		tx:   tx,
	}, TransactionExpiry)

	return &apiv1.CheckoutRes{
		TxID: txID,
	}, nil
}

func (s *Server) WriteAt(ctx context.Context, req *apiv1.WriteAtReq) (*apiv1.WriteAtRes, error) {
	awaiting := s.txStore.Get(strconv.FormatUint(uint64(req.TxID), 32)).(*awaitingTx)
	if awaiting == nil {
		return nil, errors.New("transaction ID not found")
	}
	tx := awaiting.tx
	// we want clients to handle partial writes
	n, err := tx.WriteAt(req.Data, int64(req.Offset))
	res := &apiv1.WriteAtRes{
		BytesWritten: uint32(n),
	}
	if err != nil {
		res.WriteErr = err.Error()
	}
	return res, nil
}

func (s *Server) Truncate(ctx context.Context, req *apiv1.TruncateReq) (*apiv1.Empty, error) {
	awaiting := s.txStore.Get(strconv.FormatUint(uint64(req.TxID), 32)).(*awaitingTx)
	if awaiting == nil {
		return nil, errors.New("transaction ID not found")
	}

	tx := awaiting.tx
	if err := tx.Truncate(); err != nil {
		return nil, errors.Wrap(err, "error truncating blob")
	}

	return emptyRes, nil
}

func (s *Server) PreCommit(ctx context.Context, req *apiv1.PreCommitReq) (*apiv1.PreCommitRes, error) {
	awaiting := s.txStore.Get(strconv.FormatUint(uint64(req.TxID), 32))
	if awaiting == nil {
		return nil, errors.New("transaction ID not found")
	}

	tx := awaiting.(*awaitingTx).tx
	mt, err := blob.Merkleize(blob.NewReader(tx))
	if err != nil {
		return nil, errors.Wrap(err, "error generating blob merkle root")
	}

	return &apiv1.PreCommitRes{
		MerkleRoot: mt.Root().Bytes(),
	}, nil
}

func (s *Server) Commit(ctx context.Context, req *apiv1.CommitReq) (*apiv1.CommitRes, error) {
	id := strconv.FormatUint(uint64(req.TxID), 32)
	awaiting := s.txStore.Get(id).(*awaitingTx)
	if awaiting == nil {
		return nil, errors.New("transaction ID not found")
	}

	tx := awaiting.tx
	name := tx.Name()
	info, err := store.GetNameInfo(s.db, name)
	if err != nil {
		return nil, errors.Wrap(err, "error getting name info")
	}
	mt, err := blob.Merkleize(blob.NewReader(tx))
	if err != nil {
		return nil, errors.Wrap(err, "error generating blob merkle root")
	}

	var sig crypto.Signature
	copy(sig[:], req.Signature)
	ts := time.Unix(int64(req.Timestamp), 0)
	h := blob.SealHash(name, ts, mt.Root(), crypto.ZeroHash)
	if !crypto.VerifySigPub(info.PublicKey, sig, h) {
		return nil, errors.New("signature verification failed")
	}

	if !s.nameLocker.TryLock(name) {
		return nil, errors.New("name is busy")
	}
	defer s.nameLocker.Unlock(name)

	err = store.WithTx(s.db, func(tx *leveldb.Transaction) error {
		return store.SetHeaderTx(tx, &store.Header{
			Name:         name,
			Timestamp:    ts,
			MerkleRoot:   mt.Root(),
			Signature:    sig,
			ReservedRoot: crypto.ZeroHash,
			ReceivedAt:   time.Now(),
		}, mt.ProtocolBase())
	})
	if err != nil {
		return nil, errors.Wrap(err, "error storing header")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "error committing blob")
	}
	if err := awaiting.blob.Close(); err != nil {
		return nil, errors.Wrap(err, "error closing blob")
	}

	s.txStore.Del(id)

	var recips []crypto.Hash
	if req.Broadcast {
		recips, _ = p2p.GossipAll(s.mux, &wire.Update{
			Name:       name,
			Timestamp:  ts,
			MerkleRoot: mt.Root(),
			Signature:  sig,
		})
	}
	s.lgr.Info("committed blob", "name", name, "recipient_count", len(recips))

	return &apiv1.CommitRes{}, nil
}

func (s *Server) ReadAt(_ context.Context, req *apiv1.ReadAtReq) (*apiv1.ReadAtRes, error) {
	if req.Offset > blob.Size {
		return nil, errors.New("offset is beyond blob bounds")
	}
	toRead := req.Len
	if req.Offset+toRead > blob.Size {
		return nil, errors.New("read is beyond blob bounds")
	}
	if toRead == 0 {
		return &apiv1.ReadAtRes{
			Data: make([]byte, 0),
		}, nil
	}

	name := req.Name
	if !s.nameLocker.TryRLock(name) {
		return nil, errors.New("name is busy")
	}
	defer s.nameLocker.RUnlock(name)
	bl, err := s.bs.Open(name)
	if err != nil {
		return nil, errors.Wrap(err, "error opening blob for reading")
	}
	defer bl.Close()
	buf := make([]byte, toRead)
	if _, err := bl.ReadAt(buf, int64(req.Offset)); err != nil {
		return nil, errors.Wrap(err, "error reading blob")
	}
	return &apiv1.ReadAtRes{
		Data: buf,
	}, nil
}

func (s *Server) GetBlobInfo(_ context.Context, req *apiv1.BlobInfoReq) (*apiv1.BlobInfoRes, error) {
	name := req.Name
	header, err := store.GetHeader(s.db, name)
	if err != nil {
		return nil, err
	}
	info, err := store.GetNameInfo(s.db, req.Name)
	if err != nil {
		return nil, err
	}

	return &apiv1.BlobInfoRes{
		Name:         name,
		PublicKey:    info.PublicKey.SerializeCompressed(),
		ImportHeight: uint32(info.ImportHeight),
		Timestamp:    uint64(header.Timestamp.Unix()),
		MerkleRoot:   header.MerkleRoot[:],
		ReservedRoot: header.ReservedRoot[:],
		ReceivedAt:   uint64(header.ReceivedAt.Unix()),
		Signature:    header.Signature[:],
		Timebank:     uint32(header.Timebank),
	}, nil
}

func (s *Server) ListBlobInfo(req *apiv1.ListBlobInfoReq, srv apiv1.Footnotev1_ListBlobInfoServer) error {
	stream, err := store.StreamBlobInfo(s.db, req.Start)
	if err != nil {
		return errors.Wrap(err, "error opening header stream")
	}
	defer stream.Close()

	for {
		info, err := stream.Next()
		if err != nil {
			return errors.Wrap(err, "error reading info")
		}
		if info == nil {
			return nil
		}
		res := &apiv1.BlobInfoRes{
			Name:         info.Name,
			PublicKey:    info.PublicKey.SerializeCompressed(),
			ImportHeight: uint32(info.ImportHeight),
			Timestamp:    uint64(info.Timestamp.Unix()),
			MerkleRoot:   info.MerkleRoot[:],
			ReservedRoot: info.ReservedRoot[:],
			ReceivedAt:   uint64(info.ReceivedAt.Unix()),
			Signature:    info.Signature[:],
			Timebank:     uint32(info.Timebank),
		}
		if err = srv.Send(res); err != nil {
			return errors.Wrap(err, "error sending info")
		}
	}
}

func (s *Server) SendUpdate(_ context.Context, req *apiv1.SendUpdateReq) (*apiv1.SendUpdateRes, error) {
	header, err := store.GetHeader(s.db, req.Name)
	if err != nil {
		return nil, err
	}

	recips, _ := p2p.GossipAll(s.mux, &wire.Update{
		Name:       req.Name,
		Timestamp:  header.Timestamp,
		MerkleRoot: header.MerkleRoot,
		Signature:  header.Signature,
	})

	return &apiv1.SendUpdateRes{
		RecipientCount: uint32(len(recips)),
	}, nil
}
