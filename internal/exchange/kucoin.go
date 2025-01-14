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

// StartKucoin is for starting kucoin exchange functions.
func StartKucoin(appCtx context.Context, markets []config.Market, retry *config.Retry, connCfg *config.Connection) error {

	// If any error occurs or connection is lost, retry the exchange functions with a time gap till it reaches
	// a configured number of retry.
	// Retry counter will be reset back to zero if the elapsed time since the last retry is greater than the configured one.
	var retryCount int
	lastRetryTime := time.Now()

	for {
		err := newKucoin(appCtx, markets, connCfg)
		if err != nil {
			log.Error().Err(err).Str("exchange", "kucoin").Msg("error occurred")
			if retry.Number == 0 {
				return errors.New("not able to connect kucoin exchange. please check the log for details")
			}
			if retry.ResetSec == 0 || time.Since(lastRetryTime).Seconds() < float64(retry.ResetSec) {
				retryCount++
			} else {
				retryCount = 1
			}
			lastRetryTime = time.Now()
			if retryCount > retry.Number {
				return fmt.Errorf("not able to connect kucoin exchange even after %v retry. please check the log for details", retry.Number)
			}

			log.Error().Str("exchange", "kucoin").Int("retry", retryCount).Msg(fmt.Sprintf("retrying functions in %v seconds", retry.GapSec))
			tick := time.NewTicker(time.Duration(retry.GapSec) * time.Second)
			select {
			case <-tick.C:
				tick.Stop()

			// Return, if there is any error from another exchange.
			case <-appCtx.Done():
				log.Error().Str("exchange", "kucoin").Msg("ctx canceled, return from StartKucoin")
				return appCtx.Err()
			}
		}
	}
}

type kucoin struct {
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
	wsPingIntSec   uint64
}

type wsSubKucoin struct {
	ID             int    `json:"id"`
	Type           string `json:"type"`
	Topic          string `json:"topic"`
	PrivateChannel bool   `json:"privateChannel"`
	Response       bool   `json:"response"`
}

type respKucoin struct {
	ID            string         `json:"id"`
	Topic         string         `json:"topic"`
	Data          respDataKucoin `json:"data"`
	Type          string         `json:"type"`
	mktID         string
	mktCommitName string
}

type restRespKucoin struct {
	Data []respDataKucoin `json:"data"`
}

type respDataKucoin struct {
	TradeID string      `json:"tradeId"`
	Side    string      `json:"side"`
	Size    string      `json:"size"`
	Price   string      `json:"price"`
	Time    interface{} `json:"time"`
}

type wsConnectRespKucoin struct {
	Code string `json:"code"`
	Data struct {
		Token           string `json:"token"`
		Instanceservers []struct {
			Endpoint          string `json:"endpoint"`
			Protocol          string `json:"protocol"`
			PingintervalMilli int    `json:"pingInterval"`
		} `json:"instanceServers"`
	} `json:"data"`
}

func newKucoin(appCtx context.Context, markets []config.Market, connCfg *config.Connection) error {

	// If any exchange function fails, force all the other functions to stop and return.
	kucoinErrGroup, ctx := errgroup.WithContext(appCtx)

	k := kucoin{connCfg: connCfg}

	err := k.cfgLookup(markets)
	if err != nil {
		return err
	}

	var (
		wsCount   int
		restCount int
		threshold int
	)

	for _, market := range markets {
		for _, info := range market.Info {
			switch info.Connector {
			case "websocket":
				if wsCount == 0 {

					err = k.connectWs(ctx)
					if err != nil {
						return err
					}

					kucoinErrGroup.Go(func() error {
						return k.closeWsConnOnError(ctx)
					})

					kucoinErrGroup.Go(func() error {
						return k.pingWs(ctx)
					})

					kucoinErrGroup.Go(func() error {
						return k.readWs(ctx)
					})

					if k.ter != nil {
						kucoinErrGroup.Go(func() error {
							return k.wsTickersToTerminal(ctx)
						})
						kucoinErrGroup.Go(func() error {
							return k.wsTradesToTerminal(ctx)
						})
					}

					if k.mysql != nil {
						kucoinErrGroup.Go(func() error {
							return k.wsTickersToMySQL(ctx)
						})
						kucoinErrGroup.Go(func() error {
							return k.wsTradesToMySQL(ctx)
						})
					}

					if k.es != nil {
						kucoinErrGroup.Go(func() error {
							return k.wsTickersToES(ctx)
						})
						kucoinErrGroup.Go(func() error {
							return k.wsTradesToES(ctx)
						})
					}
				}

				key := cfgLookupKey{market: market.ID, channel: info.Channel}
				val := k.cfgMap[key]
				err = k.subWsChannel(market.ID, info.Channel, val.id)
				if err != nil {
					return err
				}

				wsCount++

				// Maximum messages sent to a websocket connection per 10 sec is 100.
				// So on a safer side, this will wait for 20 sec before proceeding once it reaches ~90% of the limit.
				// (including 1 ping message so 90-1)
				threshold++
				if threshold == 89 {
					log.Debug().Str("exchange", "kucoin").Int("count", threshold).Msg("subscribe threshold reached, waiting 20 sec")
					time.Sleep(20 * time.Second)
					threshold = 0
				}

			case "rest":
				if restCount == 0 {
					err = k.connectRest()
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
				kucoinErrGroup.Go(func() error {
					return k.processREST(ctx, mktID, mktCommitName, channel, restPingIntSec)
				})

				restCount++
			}
		}
	}

	err = kucoinErrGroup.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (k *kucoin) cfgLookup(markets []config.Market) error {
	var id int

	// Configurations flat map is prepared for easy lookup later in the app.
	k.cfgMap = make(map[cfgLookupKey]cfgLookupVal)
	k.channelIds = make(map[int][2]string)
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
					if k.ter == nil {
						k.ter = storage.GetTerminal()
						k.wsTerTickers = make(chan []storage.Ticker, 1)
						k.wsTerTrades = make(chan []storage.Trade, 1)
					}
				case "mysql":
					val.mysqlStr = true
					if k.mysql == nil {
						k.mysql = storage.GetMySQL()
						k.wsMysqlTickers = make(chan []storage.Ticker, 1)
						k.wsMysqlTrades = make(chan []storage.Trade, 1)
					}
				case "elastic_search":
					val.esStr = true
					if k.es == nil {
						k.es = storage.GetElasticSearch()
						k.wsEsTickers = make(chan []storage.Ticker, 1)
						k.wsEsTrades = make(chan []storage.Trade, 1)
					}
				}
			}

			// Channel id is used to identify channel in subscribe success message of websocket server.
			id++
			k.channelIds[id] = [2]string{market.ID, info.Channel}
			val.id = id

			val.mktCommitName = marketCommitName
			k.cfgMap[key] = val
		}
	}
	return nil
}

func (k *kucoin) connectWs(ctx context.Context) error {

	// Do a REST POST request to get the websocket server details.
	resp, err := http.Post(config.KucoinRESTBaseURL+"bullet-public", "", nil)
	if err != nil {
		if !errors.Is(err, ctx.Err()) {
			logErrStack(err)
		}
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("code : %v, status : %v", resp.StatusCode, resp.Status)
	}

	r := wsConnectRespKucoin{}
	if err = jsoniter.NewDecoder(resp.Body).Decode(&r); err != nil {
		logErrStack(err)
		resp.Body.Close()
		return err
	}
	resp.Body.Close()
	if r.Code != "200000" || len(r.Data.Instanceservers) < 1 {
		return errors.New("not able to get websocket server details")
	}

	// Connect to websocket.
	ws, err := connector.NewWebsocket(ctx, &k.connCfg.WS, r.Data.Instanceservers[0].Endpoint+"?token="+r.Data.Token)
	if err != nil {
		if !errors.Is(err, ctx.Err()) {
			logErrStack(err)
		}
		return err
	}
	k.ws = ws

	frame, err := k.ws.Read()
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
		return errors.New("not able to connect websocket server")
	}

	wr := respKucoin{}
	err = jsoniter.Unmarshal(frame, &wr)
	if err != nil {
		logErrStack(err)
		return err
	}

	if wr.Type == "welcome" {
		k.wsPingIntSec = uint64(r.Data.Instanceservers[0].PingintervalMilli) / 1000
		log.Info().Str("exchange", "kucoin").Msg("websocket connected")
	} else {
		return errors.New("not able to connect websocket server")
	}
	return nil
}

// closeWsConnOnError closes websocket connection if there is any error in app context.
// This will unblock all read and writes on websocket.
func (k *kucoin) closeWsConnOnError(ctx context.Context) error {
	<-ctx.Done()
	err := k.ws.Conn.Close()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// pingWs sends ping request to websocket server for every required seconds (~10% earlier to required seconds on a safer side).
func (k *kucoin) pingWs(ctx context.Context) error {
	interval := k.wsPingIntSec * 90 / 100
	tick := time.NewTicker(time.Duration(interval) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			frame, err := jsoniter.Marshal(map[string]string{
				"id":   strconv.FormatInt(time.Now().Unix(), 10),
				"type": "ping",
			})
			if err != nil {
				logErrStack(err)
				return err
			}
			err = k.ws.Write(frame)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					err = errors.New("context canceled")
				} else {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// subWsChannel sends channel subscription requests to the websocket server.
func (k *kucoin) subWsChannel(market string, channel string, id int) error {
	switch channel {
	case "ticker":
		channel = "/market/ticker:" + market
	case "trade":
		channel = "/market/match:" + market
	}
	sub := wsSubKucoin{
		ID:             id,
		Type:           "subscribe",
		Topic:          channel,
		PrivateChannel: false,
		Response:       true,
	}
	frame, err := jsoniter.Marshal(sub)
	if err != nil {
		logErrStack(err)
		return err
	}
	err = k.ws.Write(frame)
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
func (k *kucoin) readWs(ctx context.Context) error {

	// To avoid data race, creating a new local lookup map.
	cfgLookup := make(map[cfgLookupKey]cfgLookupVal, len(k.cfgMap))
	for k, v := range k.cfgMap {
		cfgLookup[k] = v
	}

	cd := commitData{
		terTickers:   make([]storage.Ticker, 0, k.connCfg.Terminal.TickerCommitBuf),
		terTrades:    make([]storage.Trade, 0, k.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers: make([]storage.Ticker, 0, k.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:  make([]storage.Trade, 0, k.connCfg.MySQL.TradeCommitBuf),
		esTickers:    make([]storage.Ticker, 0, k.connCfg.ES.TickerCommitBuf),
		esTrades:     make([]storage.Trade, 0, k.connCfg.ES.TradeCommitBuf),
	}

	for {
		select {
		default:
			frame, err := k.ws.Read()
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

			wr := respKucoin{}
			err = jsoniter.Unmarshal(frame, &wr)
			if err != nil {
				logErrStack(err)
				return err
			}

			switch wr.Type {
			case "pong":
			case "ack":
				id, err := strconv.Atoi(wr.ID)
				if err != nil {
					logErrStack(err)
					return err
				}
				log.Debug().Str("exchange", "kucoin").Str("func", "readWs").Str("market", k.channelIds[id][0]).Str("channel", k.channelIds[id][1]).Msg("channel subscribed")
				continue
			case "message":
				s := strings.Split(wr.Topic, ":")
				if len(s) < 2 {
					continue
				}
				if s[0] == "/market/ticker" {
					wr.Topic = "ticker"
				} else {
					wr.Topic = "trade"
				}

				// Consider frame only in configured interval, otherwise ignore it.
				switch wr.Topic {
				case "ticker", "trade":
					key := cfgLookupKey{market: s[1], channel: wr.Topic}
					val := cfgLookup[key]
					if val.wsConsiderIntSec == 0 || time.Since(val.wsLastUpdated).Seconds() >= float64(val.wsConsiderIntSec) {
						val.wsLastUpdated = time.Now()
						wr.mktID = s[1]
						wr.mktCommitName = val.mktCommitName
						cfgLookup[key] = val
					} else {
						continue
					}

					err := k.processWs(ctx, &wr, &cd)
					if err != nil {
						return err
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
func (k *kucoin) processWs(ctx context.Context, wr *respKucoin, cd *commitData) error {
	switch wr.Topic {
	case "ticker":
		ticker := storage.Ticker{}
		ticker.Exchange = "kucoin"
		ticker.MktID = wr.mktID
		ticker.MktCommitName = wr.mktCommitName

		price, err := strconv.ParseFloat(wr.Data.Price, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		ticker.Price = price
		ticker.Timestamp = time.Now().UTC()

		key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
		val := k.cfgMap[key]
		if val.terStr {
			cd.terTickersCount++
			cd.terTickers = append(cd.terTickers, ticker)
			if cd.terTickersCount == k.connCfg.Terminal.TickerCommitBuf {
				select {
				case k.wsTerTickers <- cd.terTickers:
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
			if cd.mysqlTickersCount == k.connCfg.MySQL.TickerCommitBuf {
				select {
				case k.wsMysqlTickers <- cd.mysqlTickers:
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
			if cd.esTickersCount == k.connCfg.ES.TickerCommitBuf {
				select {
				case k.wsEsTickers <- cd.esTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.esTickersCount = 0
				cd.esTickers = nil
			}
		}
	case "trade":
		trade := storage.Trade{}
		trade.Exchange = "kucoin"
		trade.MktID = wr.mktID
		trade.MktCommitName = wr.mktCommitName
		trade.TradeID = wr.Data.TradeID
		trade.Side = wr.Data.Side

		size, err := strconv.ParseFloat(wr.Data.Size, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		trade.Size = size

		price, err := strconv.ParseFloat(wr.Data.Price, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		trade.Price = price

		// Time sent is in string format for websocket, int format for REST.
		if t, ok := wr.Data.Time.(string); ok {
			timestamp, err := strconv.ParseInt(t, 10, 64)
			if err != nil {
				logErrStack(err)
				return err
			}
			trade.Timestamp = time.Unix(0, timestamp*int64(time.Nanosecond)).UTC()
		} else {
			log.Error().Str("exchange", "kucoin").Str("func", "processWs").Interface("time", wr.Data.Time).Msg("")
			return errors.New("cannot convert trade data field time to string")
		}

		key := cfgLookupKey{market: trade.MktID, channel: "trade"}
		val := k.cfgMap[key]
		if val.terStr {
			cd.terTradesCount++
			cd.terTrades = append(cd.terTrades, trade)
			if cd.terTradesCount == k.connCfg.Terminal.TradeCommitBuf {
				select {
				case k.wsTerTrades <- cd.terTrades:
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
			if cd.mysqlTradesCount == k.connCfg.MySQL.TradeCommitBuf {
				select {
				case k.wsMysqlTrades <- cd.mysqlTrades:
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
			if cd.esTradesCount == k.connCfg.ES.TradeCommitBuf {
				select {
				case k.wsEsTrades <- cd.esTrades:
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

func (k *kucoin) wsTickersToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-k.wsTerTickers:
			k.ter.CommitTickers(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (k *kucoin) wsTradesToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-k.wsTerTrades:
			k.ter.CommitTrades(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (k *kucoin) wsTickersToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-k.wsMysqlTickers:
			err := k.mysql.CommitTickers(ctx, data)
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

func (k *kucoin) wsTradesToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-k.wsMysqlTrades:
			err := k.mysql.CommitTrades(ctx, data)
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

func (k *kucoin) wsTickersToES(ctx context.Context) error {
	for {
		select {
		case data := <-k.wsEsTickers:
			err := k.es.CommitTickers(ctx, data)
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

func (k *kucoin) wsTradesToES(ctx context.Context) error {
	for {
		select {
		case data := <-k.wsEsTrades:
			err := k.es.CommitTrades(ctx, data)
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

func (k *kucoin) connectRest() error {
	rest, err := connector.GetREST()
	if err != nil {
		logErrStack(err)
		return err
	}
	k.rest = rest
	log.Info().Str("exchange", "kucoin").Msg("REST connection setup is done")
	return nil
}

// processREST queries exchange for ticker / trade data through REST API in configured intervals,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (k *kucoin) processREST(ctx context.Context, mktID string, mktCommitName string, channel string, interval int) error {
	var (
		req *http.Request
		q   url.Values
		err error
	)

	cd := commitData{
		terTickers:   make([]storage.Ticker, 0, k.connCfg.Terminal.TickerCommitBuf),
		terTrades:    make([]storage.Trade, 0, k.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers: make([]storage.Ticker, 0, k.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:  make([]storage.Trade, 0, k.connCfg.MySQL.TradeCommitBuf),
		esTickers:    make([]storage.Ticker, 0, k.connCfg.ES.TickerCommitBuf),
		esTrades:     make([]storage.Trade, 0, k.connCfg.ES.TradeCommitBuf),
	}

	switch channel {
	case "ticker":
		req, err = k.rest.Request(ctx, "GET", config.KucoinRESTBaseURL+"market/orderbook/level1")
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()
		q.Add("symbol", mktID)
	case "trade":
		req, err = k.rest.Request(ctx, "GET", config.KucoinRESTBaseURL+"market/histories")
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()
		q.Add("symbol", mktID)

		// Returns 100 trades.
		// If the configured interval gap is big, then maybe it will not return all the trades
		// and if the gap is too small, maybe it will return duplicate ones.
		// Better to use websocket.
	}

	tick := time.NewTicker(time.Duration(interval) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:

			switch channel {
			case "ticker":
				req.URL.RawQuery = q.Encode()
				resp, err := k.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := respKucoin{}
				if err = jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				price, err := strconv.ParseFloat(rr.Data.Price, 64)
				if err != nil {
					logErrStack(err)
					return err
				}

				ticker := storage.Ticker{
					Exchange:      "kucoin",
					MktID:         mktID,
					MktCommitName: mktCommitName,
					Price:         price,
					Timestamp:     time.Now().UTC(),
				}

				key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
				val := k.cfgMap[key]
				if val.terStr {
					cd.terTickersCount++
					cd.terTickers = append(cd.terTickers, ticker)
					if cd.terTickersCount == k.connCfg.Terminal.TickerCommitBuf {
						k.ter.CommitTickers(cd.terTickers)
						cd.terTickersCount = 0
						cd.terTickers = nil
					}
				}
				if val.mysqlStr {
					cd.mysqlTickersCount++
					cd.mysqlTickers = append(cd.mysqlTickers, ticker)
					if cd.mysqlTickersCount == k.connCfg.MySQL.TickerCommitBuf {
						err := k.mysql.CommitTickers(ctx, cd.mysqlTickers)
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
					if cd.esTickersCount == k.connCfg.ES.TickerCommitBuf {
						err := k.es.CommitTickers(ctx, cd.esTickers)
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
				resp, err := k.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := restRespKucoin{}
				if err = jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				for i := range rr.Data {
					r := rr.Data[i]

					size, err := strconv.ParseFloat(r.Size, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					price, err := strconv.ParseFloat(r.Price, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					// Time sent is in string format for websocket, int format for REST.
					t, ok := r.Time.(float64)
					if !ok {
						log.Error().Str("exchange", "kucoin").Str("func", "processREST").Interface("time", r.Time).Msg("")
						return errors.New("cannot convert trade data field time to float")
					}

					trade := storage.Trade{
						Exchange:      "kucoin",
						MktID:         mktID,
						MktCommitName: mktCommitName,
						Side:          r.Side,
						Size:          size,
						Price:         price,
						Timestamp:     time.Unix(0, int64(t)*int64(time.Nanosecond)).UTC(),
					}

					key := cfgLookupKey{market: trade.MktID, channel: "trade"}
					val := k.cfgMap[key]
					if val.terStr {
						cd.terTradesCount++
						cd.terTrades = append(cd.terTrades, trade)
						if cd.terTradesCount == k.connCfg.Terminal.TradeCommitBuf {
							k.ter.CommitTrades(cd.terTrades)
							cd.terTradesCount = 0
							cd.terTrades = nil
						}
					}
					if val.mysqlStr {
						cd.mysqlTradesCount++
						cd.mysqlTrades = append(cd.mysqlTrades, trade)
						if cd.mysqlTradesCount == k.connCfg.MySQL.TradeCommitBuf {
							err := k.mysql.CommitTrades(ctx, cd.mysqlTrades)
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
						if cd.esTradesCount == k.connCfg.ES.TradeCommitBuf {
							err := k.es.CommitTrades(ctx, cd.esTrades)
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
