package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	_ "net/http/pprof"
	"net/url"
	"sync"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"
	"github.com/sourcegraph/jsonrpc2"
)

type Candidate struct {
	Target    int                  `json:"target"`
	Candidate *webrtc.ICECandidate `json:"candidate"`
}

type Response struct {
	Params *webrtc.SessionDescription `json:"params"`
	Result *webrtc.SessionDescription `json:"result"`
	Method string                     `json:"method"`
	Id     uint64                     `json:"id"`
	Sid    string                     `json:"sid"`
}

type TrickleResponse struct {
	Params Trickle `json:"params"`
	Method string  `json:"method"`
}

type Request struct {
	Method string           `json:"method"`
	Params *json.RawMessage `json:"params,omitempty"`
	ID     jsonrpc2.ID      `json:"id"`
	Sid    string           `json:"sid"`
}

var AddrConn string

type Connect struct {
	mu   sync.Mutex
	Conn *websocket.Conn
}

func (c *Connect) sendMess(messType int, data interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	reqBodyBytes := new(bytes.Buffer)
	json.NewEncoder(reqBodyBytes).Encode(data)

	messageBytes := reqBodyBytes.Bytes()
	return c.Conn.WriteMessage(messType, messageBytes)
}

func readMessage(c *Connect, peers map[string]*JSONSignal, logger logr.Logger, done chan struct{}) {
	//defer close(done)
	for {
		_, mess, errRead := c.Conn.ReadMessage()
		if errRead != nil {
			break
		}
		var response Request
		json.Unmarshal(mess, &response)

		if response.Method == "answer" {
			fmt.Println("got answer")
			if peers[response.Sid] != nil {
				var negotiation Negotiation
				err := json.Unmarshal(*response.Params, &negotiation)
				if err != nil {
					logger.Error(err, "Err unmarshal")
				}
				if err := peers[response.Sid].SetRemoteDescription(negotiation.Desc); err != nil {
					logger.Error(err, "Err set remote answer")
				}
			}

		} else if response.Method == "offer" {
			fmt.Println("got offer")
			if peers[response.Sid] != nil {
				var negotiation Negotiation
				err := json.Unmarshal(*response.Params, &negotiation)
				answer, err := peers[response.Sid].Answer(negotiation.Desc)
				if err != nil {
					logger.Error(err, "Err create ans")
				}

				connectionUUID := uuid.New()
				connectionID := uint64(connectionUUID.ID())

				offerJSON, _ := json.Marshal(&Negotiation{
					Desc: *answer,
				})

				params := (*json.RawMessage)(&offerJSON)

				answerMessage := &Request{
					Method: "answer",
					Params: params,
					Sid:    response.Sid,
					ID: jsonrpc2.ID{
						IsString: false,
						Str:      "",
						Num:      connectionID,
					},
				}

				errSend := c.sendMess(websocket.TextMessage, answerMessage)
				if errSend != nil {
					logger.Error(errSend, "Err send mess")
				}

				// reqBodyBytes := new(bytes.Buffer)
				// json.NewEncoder(reqBodyBytes).Encode(answerMessage)
				// messageBytes := reqBodyBytes.Bytes()
				// c.WriteMessage(websocket.TextMessage, messageBytes)
			}

		} else if response.Method == "trickle" {
			if peers[response.Sid] != nil {
				fmt.Println("got trickle")
				var trickle Trickle
				if err := json.Unmarshal(*response.Params, &trickle); err != nil {
					logger.Error(err, "Err read trickle")
				}

				err := peers[response.Sid].Trickle(trickle.Candidate, trickle.Target)
				if err != nil {
					logger.Error(err, "Err add candidate")
				}
			}
		}
	}
}

func createConnWs(address string, logger logr.Logger) *Connect {
	u := url.URL{Scheme: "ws", Host: AddrConn, Path: "/pull"}
	logger.Info("connecting to", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		logger.Error(err, "Err create conn ws")
		return nil
	}
	c.SetCloseHandler(func(code int, text string) error {
		logger.Info("Create new connect ws...")
		cn := createConnWs(address, logger)
		Conn = cn
		return nil
	})

	conn := &Connect{
		Conn: c,
	}

	return conn
}

func createPeer(peerLocal *sfu.PeerLocal, c *Connect, id string, logger logr.Logger) *JSONSignal {
	p := NewJSONSignal(peerLocal, logger)

	p.OnIceCandidate = func(candidate *webrtc.ICECandidateInit, i int) {
		if i == 0 {
			i = 1
		} else {
			i = 0
		}
		if candidate != nil {
			candidateJSON, err := json.Marshal(&Trickle{
				Candidate: *candidate,
				Target:    i,
			})

			params := (*json.RawMessage)(&candidateJSON)

			if err != nil {
				logger.Error(err, "Err candidate")
			}

			message := &jsonrpc2.Request{
				Method: "trickle",
				Params: params,
			}

			errSend := c.sendMess(websocket.TextMessage, message)
			if errSend != nil {
				logger.Error(errSend, "Err send mess")
			}

			// reqBodyBytes := new(bytes.Buffer)
			// json.NewEncoder(reqBodyBytes).Encode(message)
			// messageBytes := reqBodyBytes.Bytes()
			// c.WriteMessage(websocket.TextMessage, messageBytes)
		}
	}
	p.Join(id, "pull", sfu.JoinConfig{
		NoPublish:       false,
		NoSubscribe:     false,
		NoAutoSubscribe: false,
	})
	return p
}

func sendOfferJoin(pc *webrtc.PeerConnection, ssid string, c *Connect, logger logr.Logger) error {
	offer, err := pc.CreateOffer(nil)

	errSetDps := pc.SetLocalDescription(offer)
	if errSetDps != nil {
		logger.Error(errSetDps, "Err set dps")
		return errSetDps
	}
	offerJSON, err := json.Marshal(&Join{
		Offer: offer,
		SID:   ssid,
		UID:   "",
		Config: sfu.JoinConfig{
			NoPublish:       false,
			NoSubscribe:     false,
			NoAutoSubscribe: false,
		},
	})
	if err != nil {
		logger.Error(err, "Err create offer")
		return err
	}
	params := (*json.RawMessage)(&offerJSON)
	connectionUUID := uuid.New()
	connectionID := uint64(connectionUUID.ID())
	offerMessage := &Request{
		Method: "join",
		Params: params,
		Sid:    ssid,
		ID: jsonrpc2.ID{
			IsString: false,
			Str:      "",
			Num:      connectionID,
		},
	}

	errSend := c.sendMess(websocket.TextMessage, offerMessage)
	if errSend != nil {
		logger.Error(errSend, "Err send mess")
	}

	// reqBodyBytes := new(bytes.Buffer)
	// json.NewEncoder(reqBodyBytes).Encode(offerMessage)

	// messageBytes := reqBodyBytes.Bytes()
	// c.WriteMessage(websocket.TextMessage, messageBytes)
	return nil
}

func connectOrigin(s *sfu.SFU, sids []string, logger logr.Logger) {
	// c := createConnWs("localhost:7070", logger)
	// defer c.Close()

	// fmt.Println("Sids", sids)

	// pullPeers := make(map[string]*JSONSignal, len(sids))

	// for _, id := range sids {
	// 	p := createPeer(sfu.NewPeer(s), c, id, logger)
	// 	fmt.Println("join ssid", id, p.PeerLocal.ID())
	// 	// p.Join(id, "pull", sfu.JoinConfig{
	// 	// 	NoPublish:       false,
	// 	// 	NoSubscribe:     false,
	// 	// 	NoAutoSubscribe: false,
	// 	// })
	// 	defer p.Close()
	// 	pullPeers[id] = p
	// }

	// done := make(chan struct{})

	// go readMessage(c, pullPeers, logger, done)

	// fmt.Println("peers", pullPeers)

	// for id, p := range pullPeers {
	// 	pc := p.Subscriber().GetPeerConnection()
	// 	sendOfferJoin(pc, id, c, logger)
	// }

	// <-done
}
