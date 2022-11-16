package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	cacheredis "github.com/pion/ion-sfu/pkg/cache"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"
	"github.com/sourcegraph/jsonrpc2"
)

// Join message sent when initializing a peer connection
type Join struct {
	SID    string                    `json:"sid"`
	UID    string                    `json:"uid"`
	Offer  webrtc.SessionDescription `json:"offer"`
	Config sfu.JoinConfig            `json:"config"`
}

// Negotiation message sent when renegotiating the peer connection
type Negotiation struct {
	Desc webrtc.SessionDescription `json:"desc"`
}

// Trickle message sent when renegotiating the peer connection
type Trickle struct {
	Target    int                     `json:"target"`
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

type JSONSignal struct {
	*sfu.PeerLocal
	logr.Logger
}

var Conn *Connect
var PullPeers = make(map[string]*JSONSignal)

func NewJSONSignal(p *sfu.PeerLocal, l logr.Logger) *JSONSignal {
	return &JSONSignal{p, l}
}

// Handle incoming RPC call events like join, answer, offer and trickle
func (p *JSONSignal) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	replyError := func(err error) {
		_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
			Code:    500,
			Message: fmt.Sprintf("%s", err),
		})
	}

	switch req.Method {
	case "join":
		var join Join
		err := json.Unmarshal(*req.Params, &join)

		if err != nil {
			p.Logger.Error(err, "connect: error parsing offer")
			replyError(err)
			break
		}

		ssids, err := cacheredis.GetCacheRedis("sessionid")
		if err != nil {
			p.Logger.Error(err, "Err get cache ssid")
			break
		}

		if join.SID != "" {
			for _, id := range ssids {
				if id == join.SID {
					p.OnOffer = func(offer *webrtc.SessionDescription) {
						if err := conn.Notify(ctx, "offer", offer); err != nil {
							p.Logger.Error(err, "error sending offer")
						}

					}
					p.OnIceCandidate = func(candidate *webrtc.ICECandidateInit, target int) {
						if err := conn.Notify(ctx, "trickle", Trickle{
							Candidate: *candidate,
							Target:    target,
						}); err != nil {
							p.Logger.Error(err, "error sending ice candidate")
						}
					}

					accept, newConn, s := p.GetProvider().CheckSession(join.SID)
					if newConn == true {
						if Conn == nil {
							c := createConnWs("localhost:7070", p.Logger)
							Conn = c

							done := make(chan struct{})
							go readMessage(Conn, PullPeers, p.Logger, done)
						}

						peerPull := createPeer(sfu.NewPeer(s), Conn, join.SID, p.Logger)
						//defer peerPull.Close()
						PullPeers[join.SID] = peerPull

						pc := peerPull.Subscriber().GetPeerConnection()
						go sendOfferJoin(pc, join.SID, Conn, peerPull.Logger)

					}
					if accept == true {
						err = p.Join(join.SID, join.UID, join.Config)
						if err != nil {
							replyError(err)
							break
						}

						answer, err := p.Answer(join.Offer)
						if err != nil {
							replyError(err)
							break
						}

						_ = conn.Reply(ctx, req.ID, answer)
					} else {
						p.Close()
					}
					break
				}
			}
		} else {
			p.Close()
		}

	case "offer":
		var negotiation Negotiation
		err := json.Unmarshal(*req.Params, &negotiation)

		if err != nil {
			p.Logger.Error(err, "connect: error parsing offer")
			replyError(err)
			break
		}

		answer, err := p.Answer(negotiation.Desc)
		if err != nil {
			replyError(err)
			break
		}
		_ = conn.Reply(ctx, req.ID, answer)

	case "answer":
		var negotiation Negotiation
		err := json.Unmarshal(*req.Params, &negotiation)

		if err != nil {
			p.Logger.Error(err, "connect: error parsing answer")
			replyError(err)
			break
		}

		err = p.SetRemoteDescription(negotiation.Desc)
		if err != nil {
			replyError(err)
		}

	case "trickle":
		var trickle Trickle
		err := json.Unmarshal(*req.Params, &trickle)

		if err != nil {
			p.Logger.Error(err, "connect: error parsing candidate")
			replyError(err)
			break
		}

		err = p.Trickle(trickle.Candidate, trickle.Target)
		if err != nil {
			replyError(err)
		}
	}
}
