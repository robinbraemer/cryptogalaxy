package exchange

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/milkywaybrain/cryptogalaxy/internal/config"
	"github.com/milkywaybrain/cryptogalaxy/internal/connector"
	"github.com/milkywaybrain/cryptogalaxy/internal/storage"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

// StartBitstamp is for starting bitstamp exchange functions.
func StartBitstamp(appCtx context.Context, markets []config.Market, retry *config.Retry, connCfg *config.Connection) error {

	// If any error occurs or connection is lost, retry the exchange functions with a time gap till it reaches
	// a configured number of retry.
	// Retry counter will be reset back to zero if the elapsed time since the last retry is greater than the configured one.
	var retryCount int
	lastRetryTime := time.Now()

	for {
		err := newBitstamp(appCtx, markets, connCfg)
		if err != nil {
			log.Error().Err(err).Str("exchange", "bitstamp").Msg("error occurred")
			if retry.Number == 0 {
				return errors.New("not able to connect bitstamp exchange. please check the log for details")
			}
			if retry.ResetSec == 0 || time.Since(lastRetryTime).Seconds() < float64(retry.ResetSec) {
				retryCount++
			} else {
				retryCount = 1
			}
			lastRetryTime = time.Now()
			if retryCount > retry.Number {
				return fmt.Errorf("not able to connect bitstamp exchange even after %v retry. please check the log for details", retry.Number)
			}

			log.Error().Str("exchange", "bitstamp").Int("retry", retryCount).Msg(fmt.Sprintf("retrying functions in %v seconds", retry.GapSec))
			tick := time.NewTicker(time.Duration(retry.GapSec) * time.Second)
			select {
			case <-tick.C:
				tick.Stop()

			// Return, if there is any error from another exchange.
			case <-appCtx.Done():
				log.Error().Str("exchange", "bitstamp").Msg("ctx canceled, return from StartBitstamp")
				return appCtx.Err()
			}
		}
	}
}

type bitstamp struct {
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

type wsRespBitstamp struct {
	Event         string             `json:"event"`
	Channel       string             `json:"channel"`
	Data          wsRespDataBitstamp `json:"data"`
	mktID         string
	mktCommitName string
}

type wsRespDataBitstamp struct {
	TradeID   uint64  `json:"id"`
	Type      int     `json:"type"`
	Amount    float64 `json:"amount"`
	Price     float64 `json:"price"`
	Timestamp string  `json:"microtimestamp"`
	Channel   string  `json:"channel"`
}

type restRespBitstamp struct {
	TradeID     string `json:"tid"`
	Type        string `json:"type"`
	Amount      string `json:"amount"`
	TickerPrice string `json:"last"`
	TradePrice  string `json:"price"`
	Timestamp   string `json:"date"`
}

func newBitstamp(appCtx context.Context, markets []config.Market, connCfg *config.Connection) error {

	// If any exchange function fails, force all the other functions to stop and return.
	bitstampErrGroup, ctx := errgroup.WithContext(appCtx)

	b := bitstamp{connCfg: connCfg}

	err := b.cfgLookup(markets)
	if err != nil {
		return err
	}

	var (
		wsCount   int
		restCount int
	)
	subChannels := make(map[string]bool)

	for _, market := range markets {
		for _, info := range market.Info {
			switch info.Connector {
			case "websocket":
				if wsCount == 0 {

					err = b.connectWs(ctx)
					if err != nil {
						return err
					}

					bitstampErrGroup.Go(func() error {
						return b.closeWsConnOnError(ctx)
					})

					bitstampErrGroup.Go(func() error {
						return b.readWs(ctx)
					})

					if b.ter != nil {
						bitstampErrGroup.Go(func() error {
							return b.wsTickersToTerminal(ctx)
						})
						bitstampErrGroup.Go(func() error {
							return b.wsTradesToTerminal(ctx)
						})
					}

					if b.mysql != nil {
						bitstampErrGroup.Go(func() error {
							return b.wsTickersToMySQL(ctx)
						})
						bitstampErrGroup.Go(func() error {
							return b.wsTradesToMySQL(ctx)
						})
					}

					if b.es != nil {
						bitstampErrGroup.Go(func() error {
							return b.wsTickersToES(ctx)
						})
						bitstampErrGroup.Go(func() error {
							return b.wsTradesToES(ctx)
						})
					}
				}

				// There is only one channel provided for both ticker and trade data,
				// so need to subscribe only once.
				_, pres := subChannels[market.ID]
				if !pres {
					err = b.subWsChannel(market.ID)
					if err != nil {
						return err
					}
					subChannels[market.ID] = true
				}

				wsCount++
			case "rest":
				if restCount == 0 {
					err = b.connectRest()
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
				bitstampErrGroup.Go(func() error {
					return b.processREST(ctx, mktID, mktCommitName, channel, restPingIntSec)
				})

				restCount++
			}
		}
	}

	err = bitstampErrGroup.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (b *bitstamp) cfgLookup(markets []config.Market) error {

	// Configurations flat map is prepared for easy lookup later in the app.
	b.cfgMap = make(map[cfgLookupKey]cfgLookupVal)
	b.channelIds = make(map[int][2]string)
	for _, market := range markets {
		var mktCommitName string
		if market.CommitName != "" {
			mktCommitName = market.CommitName
		} else {
			mktCommitName = market.ID
		}
		for _, info := range market.Info {
			key := cfgLookupKey{market: market.ID, channel: info.Channel}
			val := cfgLookupVal{}
			val.wsConsiderIntSec = info.WsConsiderIntSec
			for _, str := range info.Storages {
				switch str {
				case "terminal":
					val.terStr = true
					if b.ter == nil {
						b.ter = storage.GetTerminal()
						b.wsTerTickers = make(chan []storage.Ticker, 1)
						b.wsTerTrades = make(chan []storage.Trade, 1)
					}
				case "mysql":
					val.mysqlStr = true
					if b.mysql == nil {
						b.mysql = storage.GetMySQL()
						b.wsMysqlTickers = make(chan []storage.Ticker, 1)
						b.wsMysqlTrades = make(chan []storage.Trade, 1)
					}
				case "elastic_search":
					val.esStr = true
					if b.es == nil {
						b.es = storage.GetElasticSearch()
						b.wsEsTickers = make(chan []storage.Ticker, 1)
						b.wsEsTrades = make(chan []storage.Trade, 1)
					}
				}
			}
			val.mktCommitName = mktCommitName
			b.cfgMap[key] = val
		}
	}
	return nil
}

func (b *bitstamp) connectWs(ctx context.Context) error {
	ws, err := connector.NewWebsocket(ctx, &b.connCfg.WS, config.BitstampWebsocketURL)
	if err != nil {
		if !errors.Is(err, ctx.Err()) {
			logErrStack(err)
		}
		return err
	}
	b.ws = ws
	log.Info().Str("exchange", "bitstamp").Msg("websocket connected")
	return nil
}

// closeWsConnOnError closes websocket connection if there is any error in app context.
// This will unblock all read and writes on websocket.
func (b *bitstamp) closeWsConnOnError(ctx context.Context) error {
	<-ctx.Done()
	err := b.ws.Conn.Close()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// subWsChannel sends channel subscription requests to the websocket server.
func (b *bitstamp) subWsChannel(market string) error {
	sub := wsRespBitstamp{
		Event: "bts:subscribe",
	}
	sub.Data.Channel = "live_trades_" + market
	frame, err := jsoniter.Marshal(sub)
	if err != nil {
		logErrStack(err)
		return err
	}
	err = b.ws.Write(frame)
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
func (b *bitstamp) readWs(ctx context.Context) error {

	// To avoid data race, creating a new local lookup map.
	cfgLookup := make(map[cfgLookupKey]cfgLookupVal, len(b.cfgMap))
	for k, v := range b.cfgMap {
		cfgLookup[k] = v
	}

	cd := commitData{
		terTickers:   make([]storage.Ticker, 0, b.connCfg.Terminal.TickerCommitBuf),
		terTrades:    make([]storage.Trade, 0, b.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers: make([]storage.Ticker, 0, b.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:  make([]storage.Trade, 0, b.connCfg.MySQL.TradeCommitBuf),
		esTickers:    make([]storage.Ticker, 0, b.connCfg.ES.TickerCommitBuf),
		esTrades:     make([]storage.Trade, 0, b.connCfg.ES.TradeCommitBuf),
	}

	for {
		select {
		default:
			frame, err := b.ws.Read()
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

			wr := wsRespBitstamp{}
			err = jsoniter.Unmarshal(frame, &wr)
			if err != nil {
				logErrStack(err)
				return err
			}

			if wr.Event == "bts:subscription_succeeded" || wr.Event == "trade" {
				s := strings.Split(wr.Channel, "_")
				wr.mktID = s[2]

				// There is only one channel provided for both ticker and trade data,
				// so need to duplicate it manually if there is a subscription to both the channels by user.

				// Ticker.
				key := cfgLookupKey{market: wr.mktID, channel: "ticker"}
				val, pres := cfgLookup[key]
				if pres {
					if wr.Event == "bts:subscription_succeeded" {
						log.Debug().Str("exchange", "bitstamp").Str("func", "readWs").Str("market", wr.mktID).Str("channel", "ticker").Msg("channel subscribed")
					} else if val.wsConsiderIntSec == 0 || time.Since(val.wsLastUpdated).Seconds() >= float64(val.wsConsiderIntSec) {

						// Consider frame only in configured interval, otherwise ignore it.
						val.wsLastUpdated = time.Now()
						wr.mktCommitName = val.mktCommitName
						wr.Channel = "ticker"
						cfgLookup[key] = val

						err := b.processWs(ctx, &wr, &cd)
						if err != nil {
							return err
						}
					}
				}

				// Trade.
				key = cfgLookupKey{market: wr.mktID, channel: "trade"}
				val, pres = cfgLookup[key]
				if pres {
					if wr.Event == "bts:subscription_succeeded" {
						log.Debug().Str("exchange", "bitstamp").Str("func", "readWs").Str("market", wr.mktID).Str("channel", "trade").Msg("channel subscribed")
					} else if val.wsConsiderIntSec == 0 || time.Since(val.wsLastUpdated).Seconds() >= float64(val.wsConsiderIntSec) {

						// Consider frame only in configured interval, otherwise ignore it.
						val.wsLastUpdated = time.Now()
						wr.mktCommitName = val.mktCommitName
						wr.Channel = "trade"
						cfgLookup[key] = val

						err := b.processWs(ctx, &wr, &cd)
						if err != nil {
							return err
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

// processWs receives ticker / trade data,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (b *bitstamp) processWs(ctx context.Context, wr *wsRespBitstamp, cd *commitData) error {
	switch wr.Channel {
	case "ticker":
		ticker := storage.Ticker{}
		ticker.Exchange = "bitstamp"
		ticker.MktID = wr.mktID
		ticker.MktCommitName = wr.mktCommitName
		ticker.Price = wr.Data.Price

		// Time sent is in microseconds string format.
		timestamp, err := strconv.ParseInt(wr.Data.Timestamp, 10, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		ticker.Timestamp = time.Unix(0, timestamp*int64(time.Microsecond)).UTC()

		key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
		val := b.cfgMap[key]
		if val.terStr {
			cd.terTickersCount++
			cd.terTickers = append(cd.terTickers, ticker)
			if cd.terTickersCount == b.connCfg.Terminal.TickerCommitBuf {
				select {
				case b.wsTerTickers <- cd.terTickers:
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
			if cd.mysqlTickersCount == b.connCfg.MySQL.TickerCommitBuf {
				select {
				case b.wsMysqlTickers <- cd.mysqlTickers:
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
			if cd.esTickersCount == b.connCfg.ES.TickerCommitBuf {
				select {
				case b.wsEsTickers <- cd.esTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.esTickersCount = 0
				cd.esTickers = nil
			}
		}
	case "trade":
		trade := storage.Trade{}
		trade.Exchange = "bitstamp"
		trade.MktID = wr.mktID
		trade.MktCommitName = wr.mktCommitName
		trade.TradeID = strconv.FormatUint(wr.Data.TradeID, 10)

		if wr.Data.Type == 0 {
			trade.Side = "buy"
		} else {
			trade.Side = "sell"
		}

		trade.Size = wr.Data.Amount
		trade.Price = wr.Data.Price

		// Time sent is in microseconds string format.
		timestamp, err := strconv.ParseInt(wr.Data.Timestamp, 10, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		trade.Timestamp = time.Unix(0, timestamp*int64(time.Microsecond)).UTC()

		key := cfgLookupKey{market: trade.MktID, channel: "trade"}
		val := b.cfgMap[key]
		if val.terStr {
			cd.terTradesCount++
			cd.terTrades = append(cd.terTrades, trade)
			if cd.terTradesCount == b.connCfg.Terminal.TradeCommitBuf {
				select {
				case b.wsTerTrades <- cd.terTrades:
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
			if cd.mysqlTradesCount == b.connCfg.MySQL.TradeCommitBuf {
				select {
				case b.wsMysqlTrades <- cd.mysqlTrades:
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
			if cd.esTradesCount == b.connCfg.ES.TradeCommitBuf {
				select {
				case b.wsEsTrades <- cd.esTrades:
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

func (b *bitstamp) wsTickersToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-b.wsTerTickers:
			b.ter.CommitTickers(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *bitstamp) wsTradesToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-b.wsTerTrades:
			b.ter.CommitTrades(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *bitstamp) wsTickersToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-b.wsMysqlTickers:
			err := b.mysql.CommitTickers(ctx, data)
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

func (b *bitstamp) wsTradesToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-b.wsMysqlTrades:
			err := b.mysql.CommitTrades(ctx, data)
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

func (b *bitstamp) wsTickersToES(ctx context.Context) error {
	for {
		select {
		case data := <-b.wsEsTickers:
			err := b.es.CommitTickers(ctx, data)
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

func (b *bitstamp) wsTradesToES(ctx context.Context) error {
	for {
		select {
		case data := <-b.wsEsTrades:
			err := b.es.CommitTrades(ctx, data)
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

func (b *bitstamp) connectRest() error {
	rest, err := connector.GetREST()
	if err != nil {
		logErrStack(err)
		return err
	}
	b.rest = rest
	log.Info().Str("exchange", "bitstamp").Msg("REST connection setup is done")
	return nil
}

// processREST queries exchange for ticker / trade data through REST API in configured intervals,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (b *bitstamp) processREST(ctx context.Context, mktID string, mktCommitName string, channel string, interval int) error {
	var (
		req *http.Request
		q   url.Values
		err error
	)

	cd := commitData{
		terTickers:   make([]storage.Ticker, 0, b.connCfg.Terminal.TickerCommitBuf),
		terTrades:    make([]storage.Trade, 0, b.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers: make([]storage.Ticker, 0, b.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:  make([]storage.Trade, 0, b.connCfg.MySQL.TradeCommitBuf),
		esTickers:    make([]storage.Ticker, 0, b.connCfg.ES.TickerCommitBuf),
		esTrades:     make([]storage.Trade, 0, b.connCfg.ES.TradeCommitBuf),
	}

	switch channel {
	case "ticker":
		req, err = b.rest.Request(ctx, "GET", config.BitstampRESTBaseURL+"ticker/"+mktID)
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
	case "trade":
		req, err = b.rest.Request(ctx, "GET", config.BitstampRESTBaseURL+"transactions/"+mktID)
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()

		// Querying for last 1 minute trades.
		// If the configured interval gap is big, then maybe it will not return all the trades
		// and if the gap is too small, maybe it will return duplicate ones.
		// Better to use websocket.
		q.Add("time", "minute")
	}

	tick := time.NewTicker(time.Duration(interval) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:

			switch channel {
			case "ticker":
				resp, err := b.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := restRespBitstamp{}
				if err = jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				price, err := strconv.ParseFloat(rr.TickerPrice, 64)
				if err != nil {
					logErrStack(err)
					return err
				}

				ticker := storage.Ticker{
					Exchange:      "bitstamp",
					MktID:         mktID,
					MktCommitName: mktCommitName,
					Price:         price,
					Timestamp:     time.Now().UTC(),
				}

				key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
				val := b.cfgMap[key]
				if val.terStr {
					cd.terTickersCount++
					cd.terTickers = append(cd.terTickers, ticker)
					if cd.terTickersCount == b.connCfg.Terminal.TickerCommitBuf {
						b.ter.CommitTickers(cd.terTickers)
						cd.terTickersCount = 0
						cd.terTickers = nil
					}
				}
				if val.mysqlStr {
					cd.mysqlTickersCount++
					cd.mysqlTickers = append(cd.mysqlTickers, ticker)
					if cd.mysqlTickersCount == b.connCfg.MySQL.TickerCommitBuf {
						err := b.mysql.CommitTickers(ctx, cd.mysqlTickers)
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
					if cd.esTickersCount == b.connCfg.ES.TickerCommitBuf {
						err := b.es.CommitTickers(ctx, cd.esTickers)
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
				resp, err := b.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := []restRespBitstamp{}
				if err := jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				for i := range rr {
					r := rr[i]
					var side string
					if r.Type == "0" {
						side = "buy"
					} else {
						side = "sell"
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

					// Time sent is in seconds string format.
					timestamp, err := strconv.ParseInt(r.Timestamp, 10, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					trade := storage.Trade{
						Exchange:      "bitstamp",
						MktID:         mktID,
						MktCommitName: mktCommitName,
						TradeID:       r.TradeID,
						Side:          side,
						Size:          size,
						Price:         price,
						Timestamp:     time.Unix(timestamp, 0).UTC(),
					}

					key := cfgLookupKey{market: trade.MktID, channel: "trade"}
					val := b.cfgMap[key]
					if val.terStr {
						cd.terTradesCount++
						cd.terTrades = append(cd.terTrades, trade)
						if cd.terTradesCount == b.connCfg.Terminal.TradeCommitBuf {
							b.ter.CommitTrades(cd.terTrades)
							cd.terTradesCount = 0
							cd.terTrades = nil
						}
					}
					if val.mysqlStr {
						cd.mysqlTradesCount++
						cd.mysqlTrades = append(cd.mysqlTrades, trade)
						if cd.mysqlTradesCount == b.connCfg.MySQL.TradeCommitBuf {
							err := b.mysql.CommitTrades(ctx, cd.mysqlTrades)
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
						if cd.esTradesCount == b.connCfg.ES.TradeCommitBuf {
							err := b.es.CommitTrades(ctx, cd.esTrades)
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
