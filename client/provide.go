package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-delegated-routing/gen/proto"
	"github.com/ipld/edelweiss/values"
	"github.com/ipld/go-ipld-prime/codec/dagjson"
	"github.com/ipld/go-ipld-prime/node/bindnode"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multicodec"
	"github.com/polydawn/refmt/cbor"
)

// Provider represents the source publishing one or more CIDs
type Provider struct {
	Peer          peer.AddrInfo
	ProviderProto []TransferProtocol
}

// ToProto convers a provider into the wire proto form
func (p *Provider) ToProto() *proto.Provider {
	pp := proto.Provider{
		ProviderNode: proto.Node{
			Peer: ToProtoPeer(p.Peer),
		},
		ProviderProto: proto.TransferProtocolList{},
	}
	for _, tp := range p.ProviderProto {
		pp.ProviderProto = append(pp.ProviderProto, tp.ToProto())
	}
	return &pp
}

// TransferProtocol represents a data transfer protocol
type TransferProtocol struct {
	Codec   multicodec.Code
	Payload []byte
}

// GraphSyncFILv1 is the current filecoin storage provider protocol.
type GraphSyncFILv1 struct {
	PieceCID      cid.Cid
	VerifiedDeal  bool
	FastRetrieval bool
}

// ToProto converts a TransferProtocol to the wire representation
func (tp *TransferProtocol) ToProto() proto.TransferProtocol {
	if tp.Codec == multicodec.TransportBitswap {
		return proto.TransferProtocol{
			Bitswap: &proto.BitswapProtocol{},
		}
	} else if tp.Codec == multicodec.TransportGraphsyncFilecoinv1 {
		into := GraphSyncFILv1{}
		if err := cbor.Unmarshal(cbor.DecodeOptions{}, tp.Payload, &into); err != nil {
			return proto.TransferProtocol{}
		}
		return proto.TransferProtocol{
			GraphSyncFILv1: &proto.GraphSyncFILv1Protocol{
				PieceCID:      proto.LinkToAny(into.PieceCID),
				VerifiedDeal:  values.Bool(into.VerifiedDeal),
				FastRetrieval: values.Bool(into.FastRetrieval),
			},
		}
	} else {
		return proto.TransferProtocol{}
	}
}

func parseProtocol(tp *proto.TransferProtocol) TransferProtocol {
	if tp.Bitswap != nil {
		return TransferProtocol{Codec: multicodec.TransportBitswap}
	} else if tp.GraphSyncFILv1 != nil {
		pl := GraphSyncFILv1{
			PieceCID:      cid.Cid(tp.GraphSyncFILv1.PieceCID),
			VerifiedDeal:  bool(tp.GraphSyncFILv1.VerifiedDeal),
			FastRetrieval: bool(tp.GraphSyncFILv1.FastRetrieval),
		}
		plBytes, err := cbor.Marshal(&pl)
		if err != nil {
			return TransferProtocol{}
		}
		return TransferProtocol{
			Codec:   multicodec.TransportGraphsyncFilecoinv1,
			Payload: plBytes,
		}
	}
	return TransferProtocol{}
}

// ProvideRequest is a message indicating a provider can provide a Key for a given TTL
type ProvideRequest struct {
	Key cid.Cid
	Provider
	TTL       time.Duration
	signature struct {
		At    time.Time
		Bytes []byte
	}
}

// Sign a provide request
func (pr *ProvideRequest) Sign(key crypto.PrivKey) error {
	if pr.IsSigned() {
		return errors.New("Already Signed")
	}
	pr.signature = struct {
		At    time.Time
		Bytes []byte
	}{
		At:    time.Now(),
		Bytes: []byte{},
	}

	sid, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return err
	}
	if sid != pr.Provider.Peer.ID {
		return errors.New("not the correct signing key")
	}

	node := bindnode.Wrap(pr, nil)
	nodeRepr := node.Representation()
	outBuf := bytes.NewBuffer(nil)
	if err = dagjson.Encode(nodeRepr, outBuf); err != nil {
		return err
	}
	hash := sha256.New().Sum(outBuf.Bytes())
	sig, err := key.Sign(hash)
	if err != nil {
		return err
	}
	pr.signature.Bytes = sig
	return nil
}

func (pr *ProvideRequest) Verify() error {
	if !pr.IsSigned() {
		return errors.New("Not Signed")
	}
	sig := pr.signature.Bytes
	pr.signature.Bytes = []byte{}
	defer func() {
		pr.signature.Bytes = sig
	}()

	node := bindnode.Wrap(pr, nil)
	nodeRepr := node.Representation()
	outBuf := bytes.NewBuffer(nil)
	if err := dagjson.Encode(nodeRepr, outBuf); err != nil {
		return err
	}
	hash := sha256.New().Sum(outBuf.Bytes())

	pk, err := pr.Peer.ID.ExtractPublicKey()
	if err != nil {
		return err
	}

	ok, err := pk.Verify(hash, sig)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("signature failed to verify")
	}

	return nil
}

// IsSigned indicates if the ProvideRequest has been signed
func (pr *ProvideRequest) IsSigned() bool {
	return pr.signature.Bytes != nil
}

func ParseProvideRequest(req *proto.ProvideRequest) (*ProvideRequest, error) {
	pr := ProvideRequest{
		Key:      cid.Cid(req.Key),
		Provider: parseProvider(&req.Provider),
		TTL:      time.Duration(req.AdvisoryTTL),
		signature: struct {
			At    time.Time
			Bytes []byte
		}{
			At:    time.Unix(int64(req.Timestamp), 0),
			Bytes: req.Signature,
		},
	}

	if err := pr.Verify(); err != nil {
		return nil, err
	}
	return &pr, nil
}

func parseProvider(p *proto.Provider) Provider {
	prov := Provider{
		Peer:          parseProtoNodeToAddrInfo(p.ProviderNode)[0],
		ProviderProto: make([]TransferProtocol, 0),
	}
	for _, tp := range p.ProviderProto {
		prov.ProviderProto = append(prov.ProviderProto, parseProtocol(&tp))
	}
	return prov
}

type ProvideAsyncResult struct {
	AdvisoryTTL time.Duration
	Err         error
}

// Provide makes a provide request to a delegated router
func (fp *Client) Provide(ctx context.Context, req *ProvideRequest) (<-chan ProvideAsyncResult, error) {
	if !req.IsSigned() {
		return nil, errors.New("request is not signed")
	}
	ch0, err := fp.client.Provide_Async(ctx, &proto.ProvideRequest{
		Key:         proto.LinkToAny(req.Key),
		Provider:    *req.Provider.ToProto(),
		Timestamp:   values.Int(req.signature.At.Unix()),
		AdvisoryTTL: values.Int(req.TTL),
		Signature:   req.signature.Bytes,
	})
	if err != nil {
		return nil, err
	}
	ch1 := make(chan ProvideAsyncResult, 1)
	go func() {
		defer close(ch1)
		for {
			select {
			case <-ctx.Done():
				return
			case r0, ok := <-ch0:
				if !ok {
					return
				}

				var r1 ProvideAsyncResult

				if r0.Err != nil {
					r1.Err = r0.Err
					select {
					case <-ctx.Done():
						return
					case ch1 <- r1:
					}
					continue
				}

				if r0.Resp == nil {
					continue
				}

				r1.AdvisoryTTL = time.Duration(r0.Resp.AdvisoryTTL)

				select {
				case <-ctx.Done():
					return
				case ch1 <- r1:
				}
			}
		}
	}()
	return ch1, nil
}
