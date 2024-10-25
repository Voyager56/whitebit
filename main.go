package main

import (
	"context"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// constants
const (
	urlSymbols        = "https://whitebit.com/api/v4/public/ticker"
	wsURL             = "wss://api.whitebit.com/ws"
	pingInterval      = 5 * time.Second
	minReconnectDelay = 2 * time.Second
	maxReconnectDelay = 10 * time.Second
	maxRetries        = 60
)

type Rate struct {
	From string
	To   string
	Buy  *big.Float
	Sell *big.Float
}

type Parser struct {
	idKey          string
	conn           *websocket.Conn
	symbols        []string
	reconnectCount int
	mutex          sync.RWMutex
	status         string
	done           chan bool
	emitter        func(event string, source string, rate Rate)
	pingTimer      *time.Timer
}

type (
	WSMessage struct {
		ID     int             `json:"id",omitempty`
		Method string          `json:"method"`
		Params []string        `json:"params"`
		Result map[string]bool `json:"result"`
	}

	TickerResponse map[string]struct {
		IsFrozen bool `json:"isFrozen"`
	}
)

func initParser(emmiter func(event string, source string, rate Rate)) (*Parser, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	idKey := filepath.Base(dir)

	return &Parser{
		idKey:   idKey,
		status:  "initializing",
		done:    make(chan bool),
		emitter: emmiter,
	}, nil

}

func (p *Parser) fetchSymbols() error {
	res, err := http.Get(urlSymbols)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// parse response

	var ticker TickerResponse

	if err := json.NewDecoder(res.Body).Decode(&ticker); err != nil {
		return err
	}

	ZERO := big.NewFloat(0)
	for pair, data := range ticker {
		if data.IsFrozen {
			pairs := strings.Split(pair, "_")
			if len(pairs) != 2 {
				continue
			}
			R1 := Rate{
				From: pairs[0],
				To:   pairs[1],
				Buy:  ZERO,
				Sell: ZERO,
			}
			R2 := Rate{
				From: pairs[1],
				To:   pairs[0],
				Buy:  ZERO,
				Sell: ZERO,
			}

			p.emitter("updateRate", p.idKey, R1)
			p.emitter("updateRate", p.idKey, R2)
			continue
		}
		p.symbols = append(p.symbols, pair)
	}

	return nil
}

func (p *Parser) WSConnect(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}

	p.conn = conn
	p.status = "connected"

	go p.readLoop(ctx)
	go p.writeLoop(ctx)

	if err := p.subscribe(); err != nil {
		return err
	}

	return nil
}

func (p *Parser) subscribe() error {
	msg := WSMessage{
		ID:     7,
		Method: "lastprice_subscribe",
		Params: p.symbols,
	}

	data, err := json.Marshal(msg)

	if err != nil {
		return err
	}

	if err := p.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return err
	}

	return nil

}

func (p *Parser) readLoop(ctx context.Context) {
	defer func() {
		p.conn.Close()
		p.reconnect(ctx)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		default:
			_, data, err := p.conn.ReadMessage()
			if err != nil {
				log.Printf("Error reading message: %s", err)
				return
			}

			if err := p.handleMessage(data); err != nil {
				log.Printf("Error handling message: %s", err)
				continue
			}
		}
	}

}

func (p *Parser) handleMessage(data []byte) error {
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	if msg.ID == 6 && msg.Result != nil {
		println("subscibed to price updates")
		return nil
	}

	if msg.Method == "lastprice_update" && len(msg.Params) == 2 {
		pair := msg.Params[0]
		price := msg.Params[1]

		if !strings.Contains(pair, "_") {
			return nil
		}

		return p.processPairPrices(pair, price)
	}

	return nil

}

func (p *Parser) processPairPrices(pair, priceStr string) error {
	parts := strings.Split(pair, "_")
	if len(parts) != 2 {
		return nil
	}

	price, ok := new(big.Float).SetString(priceStr)
	if !ok {
		return nil
	}

	one := big.NewFloat(1)
	inversePrice := new(big.Float).Quo(one, price)

	R1 := Rate{
		From: parts[0],
		To:   parts[1],
		Buy:  price,
		Sell: inversePrice,
	}

	R2 := Rate{
		From: parts[1],
		To:   parts[0],
		Buy:  inversePrice,
		Sell: price,
	}

	p.emitter("updateRate", p.idKey, R1)
	p.emitter("updateRate", p.idKey, R2)

	return nil

}

func (p *Parser) writeLoop(ctx context.Context) {
	p.pingTimer = time.NewTimer(pingInterval)
	defer p.pingTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-p.pingTimer.C:
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("Error sending ping: %s", err)
				return
			}
		}
	}
}

func (p *Parser) Stop() {
	close(p.done)
	if p.conn != nil {
		p.conn.Close()
	}
	if p.pingTimer != nil {
		p.pingTimer.Stop()
	}
}

func (p *Parser) Start(ctx context.Context) error {
	if err := p.fetchSymbols(); err != nil {
		return err
	}

	if err := p.WSConnect(ctx); err != nil {
		return err
	}

	return nil
}

func (p *Parser) reconnect(ctx context.Context) {
	p.mutex.Lock()
	p.reconnectCount++
	count := p.reconnectCount
	p.mutex.Unlock()

	if count > maxRetries {
		count = 5
	}

	delay := time.Duration(count) * minReconnectDelay

	if delay > maxReconnectDelay {
		delay = maxReconnectDelay
	}

	log.Printf("Reconnecting in %s", delay)

	select {
	case <-time.After(delay):
		if err := p.WSConnect(ctx); err != nil {
			log.Printf("Error reconnecting: %s", err)
			p.reconnect(ctx)
		}
	case <-ctx.Done():
		return
	case <-p.done:
		return
	}
}

func (p *Parser) Status() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.status
}

func main() {
	// Create context that can be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create rate handler function
	rateHandler := func(event, source string, rate Rate) {
		log.Printf("%s/%s: Buy: %v, Sell: %v",
			rate.From, rate.To,
			rate.Buy.String(), rate.Sell.String())
	}

	// Create parser
	p, err := initParser(rateHandler)
	if err != nil {
		log.Fatalf("Failed to create parser: %v", err)
	}

	// Start parser
	if err := p.Start(ctx); err != nil {
		log.Fatalf("Failed to start parser: %v", err)
	}
	defer p.Stop()

	// Handle shutdown gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Cleanup will happen in deferred calls
	log.Println("Shutting down...")
}
