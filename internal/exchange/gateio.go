package exchange

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/milkywaybrain/cryptogalaxy/internal/config"
	"github.com/milkywaybrain/cryptogalaxy/internal/connector"
	"github.com/milkywaybrain/cryptogalaxy/internal/storage"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

// StartGateio is for starting gateio exchange functions.
func StartGateio(appCtx context.Context, markets []config.Market, retry *config.Retry, connCfg *config.Connection) error {

	// If any error occurs or connection is lost, retry the exchange functions with a time gap till it reaches
	// a configured number of retry.
	// Retry counter will be reset back to zero if the elapsed time since the last retry is greater than the configured one.
	var retryCount int
	lastRetryTime := time.Now()

	for {
		err := newGateio(appCtx, markets, connCfg)
		if err != nil {
			log.Error().Err(err).Str("exchange", "gateio").Msg("error occurred")
			if retry.Number == 0 {
				return errors.New("not able to connect gateio exchange. please check the log for details")
			}
			if retry.ResetSec == 0 || time.Since(lastRetryTime).Seconds() < float64(retry.ResetSec) {
				retryCount++
			} else {
				retryCount = 1
			}
			lastRetryTime = time.Now()
			if retryCount > retry.Number {
				return fmt.Errorf("not able to connect gateio exchange even after %v retry. please check the log for details", retry.Number)
			}

			log.Error().Str("exchange", "gateio").Int("retry", retryCount).Msg(fmt.Sprintf("retrying functions in %v seconds", retry.GapSec))
			tick := time.NewTicker(time.Duration(retry.GapSec) * time.Second)
			select {
			case <-tick.C:
				tick.Stop()

			// Return, if there is any error from another exchange.
			case <-appCtx.Done():
				log.Error().Str("exchange", "gateio").Msg("ctx canceled, return from StartGateio")
				return appCtx.Err()
			}
		}
	}
}

type gateio struct {
	ws             connector.Websocket
	rest           *connector.REST
	connCfg        *config.Connection
	cfgMap         map[cfgLookupKey]cfgLookupVal
	channelIds     map[int][2]string
	ter            *storage.Terminal
	es             *storage.ElasticSearch
	mysql          *storage.MySQL
	wsTerTickers   chan []storage.Ticker
	wsTerTrades    chan []storage.Trade
	wsMysqlTickers chan []storage.Ticker
	wsMysqlTrades  chan []storage.Trade
	wsEsTickers    chan []storage.Ticker
	wsEsTrades     chan []storage.Trade
}

type wsSubGateio struct {
	Time    int64     `json:"time"`
	ID      int       `json:"id"`
	Channel string    `json:"channel"`
	Event   string    `json:"event"`
	Payload [1]string `json:"payload"`
}

type wsSubErrGateio struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type wsRespGateio struct {
	Channel       string      `json:"channel"`
	Result        respGateio  `json:"result"`
	TickerTime    int64       `json:"time"`
	ID            int         `json:"id"`
	Error         interface{} `json:"error"`
	mktCommitName string
}

type respGateio struct {
	CurrencyPair string      `json:"currency_pair"`
	TradeID      interface{} `json:"id"`
	Side         string      `json:"side"`
	Amount       string      `json:"amount"`
	TickerPrice  string      `json:"last"`
	TradePrice   string      `json:"price"`
	CreateTimeMs string      `json:"create_time_ms"`
	Status       string      `json:"status"`
}

func newGateio(appCtx context.Context, markets []config.Market, connCfg *config.Connection) error {

	// If any exchange function fails, force all the other functions to stop and return.
	gateioErrGroup, ctx := errgroup.WithContext(appCtx)

	g := gateio{connCfg: connCfg}

	err := g.cfgLookup(markets)
	if err != nil {
		return err
	}

	var (
		wsCount   int
		restCount int
	)

	for _, market := range markets {
		for _, info := range market.Info {
			switch info.Connector {
			case "websocket":
				if wsCount == 0 {

					err = g.connectWs(ctx)
					if err != nil {
						return err
					}

					gateioErrGroup.Go(func() error {
						return g.closeWsConnOnError(ctx)
					})

					gateioErrGroup.Go(func() error {
						return g.readWs(ctx)
					})

					if g.ter != nil {
						gateioErrGroup.Go(func() error {
							return g.wsTickersToTerminal(ctx)
						})
						gateioErrGroup.Go(func() error {
							return g.wsTradesToTerminal(ctx)
						})
					}

					if g.mysql != nil {
						gateioErrGroup.Go(func() error {
							return g.wsTickersToMySQL(ctx)
						})
						gateioErrGroup.Go(func() error {
							return g.wsTradesToMySQL(ctx)
						})
					}

					if g.es != nil {
						gateioErrGroup.Go(func() error {
							return g.wsTickersToES(ctx)
						})
						gateioErrGroup.Go(func() error {
							return g.wsTradesToES(ctx)
						})
					}
				}

				key := cfgLookupKey{market: market.ID, channel: info.Channel}
				val := g.cfgMap[key]
				err = g.subWsChannel(market.ID, info.Channel, val.id)
				if err != nil {
					return err
				}

				wsCount++
			case "rest":
				if restCount == 0 {
					err = g.connectRest()
					if err != nil {
						return err
					}
				}

				var mktCommitName string
				if market.CommitName != "" {
					mktCommitName = market.CommitName
				} else {
					mktCommitName = market.ID
				}
				mktID := market.ID
				channel := info.Channel
				restPingIntSec := info.RESTPingIntSec
				gateioErrGroup.Go(func() error {
					return g.processREST(ctx, mktID, mktCommitName, channel, restPingIntSec)
				})

				restCount++
			}
		}
	}

	err = gateioErrGroup.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (g *gateio) cfgLookup(markets []config.Market) error {
	var id int

	// Configurations flat map is prepared for easy lookup later in the app.
	g.cfgMap = make(map[cfgLookupKey]cfgLookupVal)
	g.channelIds = make(map[int][2]string)
	for _, market := range markets {
		var marketCommitName string
		if market.CommitName != "" {
			marketCommitName = market.CommitName
		} else {
			marketCommitName = market.ID
		}
		for _, info := range market.Info {
			key := cfgLookupKey{market: market.ID, channel: info.Channel}
			val := cfgLookupVal{}
			val.wsConsiderIntSec = info.WsConsiderIntSec
			for _, str := range info.Storages {
				switch str {
				case "terminal":
					val.terStr = true
					if g.ter == nil {
						g.ter = storage.GetTerminal()
						g.wsTerTickers = make(chan []storage.Ticker, 1)
						g.wsTerTrades = make(chan []storage.Trade, 1)
					}
				case "mysql":
					val.mysqlStr = true
					if g.mysql == nil {
						g.mysql = storage.GetMySQL()
						g.wsMysqlTickers = make(chan []storage.Ticker, 1)
						g.wsMysqlTrades = make(chan []storage.Trade, 1)
					}
				case "elastic_search":
					val.esStr = true
					if g.es == nil {
						g.es = storage.GetElasticSearch()
						g.wsEsTickers = make(chan []storage.Ticker, 1)
						g.wsEsTrades = make(chan []storage.Trade, 1)
					}
				}
			}

			// Channel id is used to identify channel in subscribe success message of websocket server.
			id++
			g.channelIds[id] = [2]string{market.ID, info.Channel}
			val.id = id

			val.mktCommitName = marketCommitName
			g.cfgMap[key] = val
		}
	}
	return nil
}

func (g *gateio) connectWs(ctx context.Context) error {
	ws, err := connector.NewWebsocket(ctx, &g.connCfg.WS, config.GateioWebsocketURL)
	if err != nil {
		if !errors.Is(err, ctx.Err()) {
			logErrStack(err)
		}
		return err
	}
	g.ws = ws
	log.Info().Str("exchange", "gateio").Msg("websocket connected")
	return nil
}

// closeWsConnOnError closes websocket connection if there is any error in app context.
// This will unblock all read and writes on websocket.
func (g *gateio) closeWsConnOnError(ctx context.Context) error {
	<-ctx.Done()
	err := g.ws.Conn.Close()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// subWsChannel sends channel subscription requests to the websocket server.
func (g *gateio) subWsChannel(market string, channel string, id int) error {
	switch channel {
	case "ticker":
		channel = "spot.tickers"
	case "trade":
		channel = "spot.trades"
	}
	sub := wsSubGateio{
		Time:    time.Now().UTC().Unix(),
		ID:      id,
		Channel: channel,
		Event:   "subscribe",
		Payload: [1]string{market},
	}
	frame, err := jsoniter.Marshal(sub)
	if err != nil {
		logErrStack(err)
		return err
	}
	err = g.ws.Write(frame)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			err = errors.New("context canceled")
		} else {
			logErrStack(err)
		}
		return err
	}
	return nil
}

// readWs reads ticker / trade data from websocket channels.
func (g *gateio) readWs(ctx context.Context) error {

	// To avoid data race, creating a new local lookup map.
	cfgLookup := make(map[cfgLookupKey]cfgLookupVal, len(g.cfgMap))
	for k, v := range g.cfgMap {
		cfgLookup[k] = v
	}

	cd := commitData{
		terTickers:   make([]storage.Ticker, 0, g.connCfg.Terminal.TickerCommitBuf),
		terTrades:    make([]storage.Trade, 0, g.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers: make([]storage.Ticker, 0, g.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:  make([]storage.Trade, 0, g.connCfg.MySQL.TradeCommitBuf),
		esTickers:    make([]storage.Ticker, 0, g.connCfg.ES.TickerCommitBuf),
		esTrades:     make([]storage.Trade, 0, g.connCfg.ES.TradeCommitBuf),
	}

	for {
		select {
		default:
			frame, err := g.ws.Read()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					err = errors.New("context canceled")
				} else {
					if err == io.EOF {
						err = errors.Wrap(err, "connection close by exchange server")
					}
					logErrStack(err)
				}
				return err
			}
			if len(frame) == 0 {
				continue
			}

			wr := wsRespGateio{}
			err = jsoniter.Unmarshal(frame, &wr)
			if err != nil {
				logErrStack(err)
				return err
			}

			if wr.ID != 0 {
				if wr.Result.Status == "success" {
					log.Debug().Str("exchange", "gateio").Str("func", "readWs").Str("market", g.channelIds[wr.ID][0]).Str("channel", g.channelIds[wr.ID][1]).Msg("channel subscribed")
					continue
				} else {
					wsSubErr, ok := wr.Error.(wsSubErrGateio)
					if !ok {
						log.Error().Str("exchange", "gateio").Str("func", "processWs").Interface("error", wr.Error).Msg("")
						return errors.New("cannot convert ws resp error to error field")
					}
					log.Error().Str("exchange", "gateio").Str("func", "readWs").Int("code", wsSubErr.Code).Str("msg", wsSubErr.Message).Msg("")
					return errors.New("gateio websocket error")
				}
			}

			if wr.Channel == "spot.tickers" {
				wr.Channel = "ticker"
			} else {
				wr.Channel = "trade"
			}

			// Consider frame only in configured interval, otherwise ignore it.
			switch wr.Channel {
			case "ticker", "trade":
				key := cfgLookupKey{market: wr.Result.CurrencyPair, channel: wr.Channel}
				val := cfgLookup[key]
				if val.wsConsiderIntSec == 0 || time.Since(val.wsLastUpdated).Seconds() >= float64(val.wsConsiderIntSec) {
					val.wsLastUpdated = time.Now()
					wr.mktCommitName = val.mktCommitName
					cfgLookup[key] = val
				} else {
					continue
				}

				err := g.processWs(ctx, &wr, &cd)
				if err != nil {
					return err
				}
			}

		// Return, if there is any error from another function or exchange.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// processWs receives ticker / trade data,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (g *gateio) processWs(ctx context.Context, wr *wsRespGateio, cd *commitData) error {
	switch wr.Channel {
	case "ticker":
		ticker := storage.Ticker{}
		ticker.Exchange = "gateio"
		ticker.MktID = wr.Result.CurrencyPair
		ticker.MktCommitName = wr.mktCommitName

		price, err := strconv.ParseFloat(wr.Result.TickerPrice, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		ticker.Price = price

		// Time sent is in seconds.
		ticker.Timestamp = time.Unix(wr.TickerTime, 0).UTC()

		key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
		val := g.cfgMap[key]
		if val.terStr {
			cd.terTickersCount++
			cd.terTickers = append(cd.terTickers, ticker)
			if cd.terTickersCount == g.connCfg.Terminal.TickerCommitBuf {
				select {
				case g.wsTerTickers <- cd.terTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.terTickersCount = 0
				cd.terTickers = nil
			}
		}
		if val.mysqlStr {
			cd.mysqlTickersCount++
			cd.mysqlTickers = append(cd.mysqlTickers, ticker)
			if cd.mysqlTickersCount == g.connCfg.MySQL.TickerCommitBuf {
				select {
				case g.wsMysqlTickers <- cd.mysqlTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.mysqlTickersCount = 0
				cd.mysqlTickers = nil
			}
		}
		if val.esStr {
			cd.esTickersCount++
			cd.esTickers = append(cd.esTickers, ticker)
			if cd.esTickersCount == g.connCfg.ES.TickerCommitBuf {
				select {
				case g.wsEsTickers <- cd.esTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.esTickersCount = 0
				cd.esTickers = nil
			}
		}
	case "trade":
		trade := storage.Trade{}
		trade.Exchange = "gateio"
		trade.MktID = wr.Result.CurrencyPair
		trade.MktCommitName = wr.mktCommitName

		// Trade ID sent is in int format for websocket, string format for REST.
		if tradeIDFloat, ok := wr.Result.TradeID.(float64); ok {
			trade.TradeID = strconv.FormatFloat(tradeIDFloat, 'f', 0, 64)
		} else {
			log.Error().Str("exchange", "gateio").Str("func", "processWs").Interface("trade id", wr.Result.TradeID).Msg("")
			return errors.New("cannot convert trade data field trade id to float")
		}

		trade.Side = wr.Result.Side

		size, err := strconv.ParseFloat(wr.Result.Amount, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		trade.Size = size

		price, err := strconv.ParseFloat(wr.Result.TradePrice, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		trade.Price = price

		// Time sent is in fractional seconds string format.
		timeFloat, err := strconv.ParseFloat(wr.Result.CreateTimeMs, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		intPart, _ := math.Modf(timeFloat)
		trade.Timestamp = time.Unix(0, int64(intPart)*int64(time.Millisecond)).UTC()

		key := cfgLookupKey{market: trade.MktID, channel: "trade"}
		val := g.cfgMap[key]
		if val.terStr {
			cd.terTradesCount++
			cd.terTrades = append(cd.terTrades, trade)
			if cd.terTradesCount == g.connCfg.Terminal.TradeCommitBuf {
				select {
				case g.wsTerTrades <- cd.terTrades:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.terTradesCount = 0
				cd.terTrades = nil
			}
		}
		if val.mysqlStr {
			cd.mysqlTradesCount++
			cd.mysqlTrades = append(cd.mysqlTrades, trade)
			if cd.mysqlTradesCount == g.connCfg.MySQL.TradeCommitBuf {
				select {
				case g.wsMysqlTrades <- cd.mysqlTrades:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.mysqlTradesCount = 0
				cd.mysqlTrades = nil
			}
		}
		if val.esStr {
			cd.esTradesCount++
			cd.esTrades = append(cd.esTrades, trade)
			if cd.esTradesCount == g.connCfg.ES.TradeCommitBuf {
				select {
				case g.wsEsTrades <- cd.esTrades:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.esTradesCount = 0
				cd.esTrades = nil
			}
		}
	}
	return nil
}

func (g *gateio) wsTickersToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-g.wsTerTickers:
			g.ter.CommitTickers(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gateio) wsTradesToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-g.wsTerTrades:
			g.ter.CommitTrades(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gateio) wsTickersToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-g.wsMysqlTickers:
			err := g.mysql.CommitTickers(ctx, data)
			if err != nil {
				if !errors.Is(err, ctx.Err()) {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gateio) wsTradesToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-g.wsMysqlTrades:
			err := g.mysql.CommitTrades(ctx, data)
			if err != nil {
				if !errors.Is(err, ctx.Err()) {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gateio) wsTickersToES(ctx context.Context) error {
	for {
		select {
		case data := <-g.wsEsTickers:
			err := g.es.CommitTickers(ctx, data)
			if err != nil {
				if !errors.Is(err, ctx.Err()) {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gateio) wsTradesToES(ctx context.Context) error {
	for {
		select {
		case data := <-g.wsEsTrades:
			err := g.es.CommitTrades(ctx, data)
			if err != nil {
				if !errors.Is(err, ctx.Err()) {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gateio) connectRest() error {
	rest, err := connector.GetREST()
	if err != nil {
		logErrStack(err)
		return err
	}
	g.rest = rest
	log.Info().Str("exchange", "gateio").Msg("REST connection setup is done")
	return nil
}

// processREST queries exchange for ticker / trade data through REST API in configured intervals,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (g *gateio) processREST(ctx context.Context, mktID string, mktCommitName string, channel string, interval int) error {
	var (
		req *http.Request
		q   url.Values
		err error
	)

	cd := commitData{
		terTickers:   make([]storage.Ticker, 0, g.connCfg.Terminal.TickerCommitBuf),
		terTrades:    make([]storage.Trade, 0, g.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers: make([]storage.Ticker, 0, g.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:  make([]storage.Trade, 0, g.connCfg.MySQL.TradeCommitBuf),
		esTickers:    make([]storage.Ticker, 0, g.connCfg.ES.TickerCommitBuf),
		esTrades:     make([]storage.Trade, 0, g.connCfg.ES.TradeCommitBuf),
	}

	switch channel {
	case "ticker":
		req, err = g.rest.Request(ctx, "GET", config.GateioRESTBaseURL+"spot/tickers")
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()
		q.Add("currency_pair", mktID)
	case "trade":
		req, err = g.rest.Request(ctx, "GET", config.GateioRESTBaseURL+"spot/trades")
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()
		q.Add("currency_pair", mktID)

		// Querying for 100 trades.
		// If the configured interval gap is big, then maybe it will not return all the trades
		// and if the gap is too small, maybe it will return duplicate ones.
		// Better to use websocket.
		q.Add("limit", strconv.Itoa(100))
	}

	tick := time.NewTicker(time.Duration(interval) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:

			switch channel {
			case "ticker":
				req.URL.RawQuery = q.Encode()
				resp, err := g.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				r := []respGateio{}
				if err = jsoniter.NewDecoder(resp.Body).Decode(&r); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				rr := r[0]

				price, err := strconv.ParseFloat(rr.TickerPrice, 64)
				if err != nil {
					logErrStack(err)
					return err
				}

				ticker := storage.Ticker{
					Exchange:      "gateio",
					MktID:         mktID,
					MktCommitName: mktCommitName,
					Price:         price,
					Timestamp:     time.Now().UTC(),
				}

				key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
				val := g.cfgMap[key]
				if val.terStr {
					cd.terTickersCount++
					cd.terTickers = append(cd.terTickers, ticker)
					if cd.terTickersCount == g.connCfg.Terminal.TickerCommitBuf {
						g.ter.CommitTickers(cd.terTickers)
						cd.terTickersCount = 0
						cd.terTickers = nil
					}
				}
				if val.mysqlStr {
					cd.mysqlTickersCount++
					cd.mysqlTickers = append(cd.mysqlTickers, ticker)
					if cd.mysqlTickersCount == g.connCfg.MySQL.TickerCommitBuf {
						err := g.mysql.CommitTickers(ctx, cd.mysqlTickers)
						if err != nil {
							if !errors.Is(err, ctx.Err()) {
								logErrStack(err)
							}
							return err
						}
						cd.mysqlTickersCount = 0
						cd.mysqlTickers = nil
					}
				}
				if val.esStr {
					cd.esTickersCount++
					cd.esTickers = append(cd.esTickers, ticker)
					if cd.esTickersCount == g.connCfg.ES.TickerCommitBuf {
						err := g.es.CommitTickers(ctx, cd.esTickers)
						if err != nil {
							if !errors.Is(err, ctx.Err()) {
								logErrStack(err)
							}
							return err
						}
						cd.esTickersCount = 0
						cd.esTickers = nil
					}
				}
			case "trade":
				req.URL.RawQuery = q.Encode()
				resp, err := g.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := []respGateio{}
				if err = jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				for i := range rr {
					r := rr[i]

					// Trade ID sent is in int format for websocket, string format for REST.
					tradeID, ok := r.TradeID.(string)
					if !ok {
						log.Error().Str("exchange", "gateio").Str("func", "processREST").Interface("trade id", r.TradeID).Msg("")
						return errors.New("cannot convert trade data field trade id to string")
					}

					size, err := strconv.ParseFloat(r.Amount, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					price, err := strconv.ParseFloat(r.TradePrice, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					// Time sent is in fractional seconds string format.
					timeFloat, err := strconv.ParseFloat(r.CreateTimeMs, 64)
					if err != nil {
						logErrStack(err)
						return err
					}
					intPart, _ := math.Modf(timeFloat)
					timestamp := time.Unix(0, int64(intPart)*int64(time.Millisecond)).UTC()

					trade := storage.Trade{
						Exchange:      "gateio",
						MktID:         mktID,
						MktCommitName: mktCommitName,
						TradeID:       tradeID,
						Side:          r.Side,
						Size:          size,
						Price:         price,
						Timestamp:     timestamp,
					}

					key := cfgLookupKey{market: trade.MktID, channel: "trade"}
					val := g.cfgMap[key]
					if val.terStr {
						cd.terTradesCount++
						cd.terTrades = append(cd.terTrades, trade)
						if cd.terTradesCount == g.connCfg.Terminal.TradeCommitBuf {
							g.ter.CommitTrades(cd.terTrades)
							cd.terTradesCount = 0
							cd.terTrades = nil
						}
					}
					if val.mysqlStr {
						cd.mysqlTradesCount++
						cd.mysqlTrades = append(cd.mysqlTrades, trade)
						if cd.mysqlTradesCount == g.connCfg.MySQL.TradeCommitBuf {
							err := g.mysql.CommitTrades(ctx, cd.mysqlTrades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.mysqlTradesCount = 0
							cd.mysqlTrades = nil
						}
					}
					if val.esStr {
						cd.esTradesCount++
						cd.esTrades = append(cd.esTrades, trade)
						if cd.esTradesCount == g.connCfg.ES.TradeCommitBuf {
							err := g.es.CommitTrades(ctx, cd.esTrades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.esTradesCount = 0
							cd.esTrades = nil
						}
					}
				}
			}

		// Return, if there is any error from another function or exchange.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
