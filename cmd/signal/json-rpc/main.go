// Package cmd contains an entrypoint for running an ion-sfu instance.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/pion/ion-sfu/cmd/signal/json-rpc/server"
	log "github.com/pion/ion-sfu/pkg/logger"
	"github.com/pion/ion-sfu/pkg/middlewares/datachannel"
	"github.com/pion/ion-sfu/pkg/pull"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sourcegraph/jsonrpc2"
	websocketjsonrpc2 "github.com/sourcegraph/jsonrpc2/websocket"
	"github.com/spf13/viper"
)

// logC need to get logger options from config
type logC struct {
	Config log.GlobalConfig `mapstructure:"log"`
}

var (
	conf           = sfu.Config{}
	file           string
	cert           string
	key            string
	addr           string
	metricsAddr    string
	verbosityLevel int
	logConfig      logC
	logger         = log.New()
)

const (
	portRangeLimit = 100
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
}

type TrickleResponse struct {
	Params server.Trickle `json:"params"`
	Method string         `json:"method"`
}

func showHelp() {
	fmt.Printf("Usage:%s {params}\n", os.Args[0])
	fmt.Println("      -c {config file}")
	fmt.Println("      -cert {cert file}")
	fmt.Println("      -key {key file}")
	fmt.Println("      -a {listen addr}")
	fmt.Println("      -h (show help info)")
	fmt.Println("      -v {0-10} (verbosity level, default 0)")
}

func load() bool {
	_, err := os.Stat(file)
	if err != nil {
		return false
	}

	viper.SetConfigFile(file)
	viper.SetConfigType("toml")

	err = viper.ReadInConfig()
	if err != nil {
		logger.Error(err, "config file read failed", "file", file)
		return false
	}
	err = viper.GetViper().Unmarshal(&conf)
	if err != nil {
		logger.Error(err, "sfu config file loaded failed", "file", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) > 2 {
		logger.Error(nil, "config file loaded failed. webrtc port must be [min,max]", "file", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) != 0 && conf.WebRTC.ICEPortRange[1]-conf.WebRTC.ICEPortRange[0] < portRangeLimit {
		logger.Error(nil, "config file loaded failed. webrtc port must be [min, max] and max - min >= portRangeLimit", "file", file, "portRangeLimit", portRangeLimit)
		return false
	}

	if len(conf.Turn.PortRange) > 2 {
		logger.Error(nil, "config file loaded failed. turn port must be [min,max]", "file", file)
		return false
	}

	if logConfig.Config.V < 0 {
		logger.Error(nil, "Logger V-Level cannot be less than 0")
		return false
	}

	logger.V(0).Info("Config file loaded", "file", file)
	return true
}

func parse() bool {
	flag.StringVar(&file, "c", "config.toml", "config file")
	flag.StringVar(&cert, "cert", "", "cert file")
	flag.StringVar(&key, "key", "", "key file")
	flag.StringVar(&addr, "a", ":7000", "address to use")
	flag.StringVar(&metricsAddr, "m", ":8100", "merics to use")
	flag.IntVar(&verbosityLevel, "v", -1, "verbosity level, higher value - more logs")
	help := flag.Bool("h", false, "help info")
	flag.Parse()
	if !load() {
		return false
	}

	if *help {
		return false
	}
	return true
}

func startMetrics(addr string) {
	// start metrics server
	m := http.NewServeMux()
	m.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Handler: m,
	}

	metricsLis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error(err, "cannot bind to metrics endpoint", "addr", addr)
		os.Exit(1)
	}
	logger.Info("Metrics Listening", "addr", addr)

	err = srv.Serve(metricsLis)
	if err != nil {
		logger.Error(err, "Metrics server stopped")
	}
}

func readMessage(c *websocket.Conn, p *server.JSONSignal, done chan struct{}) {
	defer close(done)
	for {
		_, mess, errRead := c.ReadMessage()
		if errRead != nil {
			break
		}
		var response Response
		json.Unmarshal(mess, &response)
		if response.Result != nil {
			fmt.Println("got answer")
			result := *response.Result
			if err := p.SetRemoteDescription(result); err != nil {
				logger.Error(err, "Err set remote answer")
			}
		} else if response.Method == "offer" {
			fmt.Println("got offer")
			answer, err := p.Answer(*response.Params)
			if err != nil {
				logger.Error(err, "Err create ans")
			}

			connectionUUID := uuid.New()
			connectionID := uint64(connectionUUID.ID())

			offerJSON, _ := json.Marshal(&server.Negotiation{
				Desc: *answer,
			})

			params := (*json.RawMessage)(&offerJSON)

			answerMessage := &jsonrpc2.Request{
				Method: "answer",
				Params: params,
				ID: jsonrpc2.ID{
					IsString: false,
					Str:      "",
					Num:      connectionID,
				},
			}

			reqBodyBytes := new(bytes.Buffer)
			json.NewEncoder(reqBodyBytes).Encode(answerMessage)

			messageBytes := reqBodyBytes.Bytes()
			c.WriteMessage(websocket.TextMessage, messageBytes)
		} else if response.Method == "trickle" {
			fmt.Println("got trickle")
			var trickleResponse TrickleResponse
			if err := json.Unmarshal(mess, &trickleResponse); err != nil {
				logger.Error(err, "Err read trickle")
			}
			err := p.Trickle(trickleResponse.Params.Candidate, trickleResponse.Params.Target)
			if err != nil {
				logger.Error(err, "Err add candidate")
			}
		}
	}
}

func connectOrigin(s *sfu.SFU) {
	var addrConn string
	flag.StringVar(&addrConn, "add", "localhost:7070", "address to use")
	flag.Parse()
	u := url.URL{Scheme: "ws", Host: addrConn, Path: "/ws"}
	logger.Info("connecting to", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)

	p := server.NewJSONSignal(sfu.NewPeer(s), logger)
	// p.SetID(cuid.New())
	// defer p.Close()
	// ss, cfg := p.GetProvider().GetSession("test room")
	// p.SetSession(ss)
	// p.AddPeer(p)

	p.OnIceCandidate = func(candidate *webrtc.ICECandidateInit, i int) {
		if candidate != nil {
			candidateJSON, err := json.Marshal(&server.Trickle{
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

			reqBodyBytes := new(bytes.Buffer)
			json.NewEncoder(reqBodyBytes).Encode(message)

			messageBytes := reqBodyBytes.Bytes()
			c.WriteMessage(websocket.TextMessage, messageBytes)
		}
	}

	p.Join("test room", "", sfu.JoinConfig{
		NoPublish:       false,
		NoSubscribe:     false,
		NoAutoSubscribe: false,
	})

	pc := p.Subscriber().GetPeerConnection()

	if err != nil {
		logger.Error(err, "Error connect origin")
	}
	defer c.Close()
	done := make(chan struct{})

	// config := webrtc.Configuration{
	// 	ICEServers: []webrtc.ICEServer{
	// 		{
	// 			URLs: []string{"stun:stun.l.google.com:19302"},
	// 		},
	// 	},
	// 	// SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	// }
	// me, errMedia := sfu.GetMediaEngine()
	// if errMedia != nil {
	// 	logger.Error(errMedia, "Err get media")
	// 	return
	// }
	// api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(cfg.Setting))
	// pc, errPeer := api.NewPeerConnection(cfg.Configuration)
	// if errPeer != nil {
	// 	logger.Error(errPeer, "Err create peer")
	// 	return
	// }

	go readMessage(c, p, done)

	// pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
	// 	if candidate != nil {
	// 		candidateJSON, err := json.Marshal(&Candidate{
	// 			Candidate: candidate,
	// 			Target:    0,
	// 		})

	// 		params := (*json.RawMessage)(&candidateJSON)

	// 		if err != nil {
	// 			logger.Error(err, "Err candidate")
	// 		}

	// 		message := &jsonrpc2.Request{
	// 			Method: "trickle",
	// 			Params: params,
	// 		}

	// 		reqBodyBytes := new(bytes.Buffer)
	// 		json.NewEncoder(reqBodyBytes).Encode(message)

	// 		messageBytes := reqBodyBytes.Bytes()
	// 		c.WriteMessage(websocket.TextMessage, messageBytes)
	// 	}
	// })
	// pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
	// 	fmt.Printf("Connection State has changed to %s \n", connectionState.String())
	// })

	offer, _ := pc.CreateOffer(nil)

	errSetDps := pc.SetLocalDescription(offer)
	if errSetDps != nil {
		logger.Error(errSetDps, "Err set dps")
	}
	offerJSON, err := json.Marshal(&server.Join{
		Offer: offer,
		SID:   "test room",
		UID:   "",
		Config: sfu.JoinConfig{
			NoPublish:       false,
			NoSubscribe:     false,
			NoAutoSubscribe: false,
		},
	})
	params := (*json.RawMessage)(&offerJSON)
	connectionUUID := uuid.New()
	connectionID := uint64(connectionUUID.ID())
	offerMessage := &jsonrpc2.Request{
		Method: "join",
		Params: params,
		ID: jsonrpc2.ID{
			IsString: false,
			Str:      "",
			Num:      connectionID,
		},
	}
	reqBodyBytes := new(bytes.Buffer)
	json.NewEncoder(reqBodyBytes).Encode(offerMessage)

	messageBytes := reqBodyBytes.Bytes()
	c.WriteMessage(websocket.TextMessage, messageBytes)

	<-done
}

func main() {

	if !parse() {
		showHelp()
		os.Exit(-1)
	}

	// Check that the -v is not set (default -1)
	if verbosityLevel < 0 {
		verbosityLevel = logConfig.Config.V
	}

	log.SetGlobalOptions(log.GlobalConfig{V: verbosityLevel})
	logger.Info("--- Starting SFU Node ---")

	// Pass logr instance
	sfu.Logger = logger
	s := sfu.NewSFU(conf)
	dc := s.NewDatachannel(sfu.APIChannelLabel)
	dc.Use(datachannel.SubscriberAPI)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	http.Handle("/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Handle req ws", r.Method)
		c, err := upgrader.Upgrade(w, r, nil)

		if err != nil {
			panic(err)
		}
		defer c.Close()

		p := server.NewJSONSignal(sfu.NewPeer(s), logger)
		defer p.Close()

		jc := jsonrpc2.NewConn(r.Context(), websocketjsonrpc2.NewObjectStream(c), p)
		<-jc.DisconnectNotify()
	}))

	go startMetrics(metricsAddr)

	go pull.ConnectOrigin(s, logger)

	var err error
	if key != "" && cert != "" {
		logger.Info("Started listening", "addr", "https://"+addr)
		err = http.ListenAndServeTLS(addr, cert, key, nil)
	} else {
		logger.Info("Started listening", "addr", "http://"+addr)
		err = http.ListenAndServe(addr, nil)
	}
	if err != nil {
		panic(err)
	}

}
