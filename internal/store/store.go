// Package store implements Mongo persistence with envelope
// encryption for credentials: master key from env, ciphertext in DB.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
	"github.com/Abhijeetsng97/terminalTrade/internal/engine"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Collections per the spec data model.
const (
	ColUsers       = "users"
	ColCreds       = "broker_credentials"
	ColInstruments = "instruments"
	ColOrders      = "orders"
	ColTrades      = "trades"
	ColAudit       = "audit_log"
	ColConfig      = "app_config"
)

// Store wraps the Mongo database.
type Store struct {
	db *mongo.Database
	// master AES-GCM key from env (never stored).
	gcm cipher.AEAD
}

// Connect opens the Mongo database and ensures indexes.
func Connect(ctx context.Context, uri, dbname string, masterKey []byte) (*Store, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}
	db := client.Database(dbname)

	if len(masterKey) != 32 {
		return nil, fmt.Errorf("encryption master key must be 32 bytes (TT_ENCRYPTION_KEY), got %d", len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, gcm: gcm}
	if err := s.ensureIndexes(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureIndexes(ctx context.Context) error {
	// orders: parents and children in one collection, by id.
	orders := s.db.Collection(ColOrders)
	_, err := orders.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "id", Value: 1}},
	})
	if err != nil {
		return err
	}
	_, err = orders.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "parentid", Value: 1}},
	})
	if err != nil {
		return err
	}
	_, err = s.db.Collection(ColAudit).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "time", Value: -1}},
	})
	return err
}

// Close disconnects the client.
func (s *Store) Close(ctx context.Context) error {
	return s.db.Client().Disconnect(ctx)
}

// ---- envelope encryption ----

func (s *Store) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := s.gcm.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

func (s *Store) Decrypt(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(raw) < s.gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return s.gcm.Open(nil, raw[:s.gcm.NonceSize()], raw[s.gcm.NonceSize():], nil)
}

// ---- engine.Store implementation ----

type parentDoc struct {
	ID         string    `bson:"id"`
	UserID     string    `bson:"user_id"`
	Instrument bson.Raw  `bson:"instrument"`
	Side       string    `bson:"side"`
	Lots       int       `bson:"lots"`
	TotalQty   int       `bson:"total_qty"`
	OrderType    string    `bson:"order_type"`
	LimitPrice   float64   `bson:"limit_price"`
	TriggerPrice float64   `bson:"trigger_price"`
	Broker       string    `bson:"broker"`
	State        string    `bson:"state"`
	Reason       string    `bson:"reason"`
	FilledQty    int       `bson:"filled_qty"`
	Created      time.Time `bson:"created"`
	Updated      time.Time `bson:"updated"`
}

type childDoc struct {
	ID             string    `bson:"id"`
	ParentID       string    `bson:"parentid"`
	UserID         string    `bson:"user_id"`
	Instrument     bson.Raw  `bson:"instrument"`
	Side           string    `bson:"side"`
	Qty            int       `bson:"qty"`
	OrderType      string    `bson:"order_type"`
	LimitPrice     float64   `bson:"limit_price"`
	TriggerPrice   float64   `bson:"trigger_price"`
	IdempotencyTag string    `bson:"idem_tag"`
	Broker         string    `bson:"broker"`
	BrokerOrderID  string    `bson:"broker_order_id,omitempty"`
	State          string    `bson:"state"`
	FilledQty      int       `bson:"filled_qty"`
	Reason         string    `bson:"reason"`
	Created        time.Time `bson:"created"`
	Updated        time.Time `bson:"updated"`
}

// SaveParent upserts a parent order.
func (s *Store) SaveParent(ctx context.Context, o core.Order) error {
	inst, err := bson.Marshal(o.Instrument)
	if err != nil {
		return err
	}
	doc := parentDoc{
		ID: o.ID, UserID: o.UserID, Instrument: inst, Side: string(o.Side),
		Lots: o.Lots, TotalQty: o.TotalQty, OrderType: string(o.OrderType),
		LimitPrice: o.LimitPrice, TriggerPrice: o.TriggerPrice, Broker: string(o.Broker),
		State: string(o.State), Reason: o.Reason, FilledQty: o.FilledQty,
		Created: o.Created, Updated: o.Updated,
	}
	_, err = s.db.Collection(ColOrders).ReplaceOne(ctx,
		bson.D{{Key: "id", Value: o.ID}},
		doc, options.Replace().SetUpsert(true))
	return err
}

// SaveChild upserts a child slice.
func (s *Store) SaveChild(ctx context.Context, c core.ChildOrder) error {
	inst, err := bson.Marshal(c.Instrument)
	if err != nil {
		return err
	}
	doc := childDoc{
		ID: c.ID, ParentID: c.ParentID, UserID: c.UserID, Instrument: inst,
		Side: string(c.Side), Qty: c.Qty, OrderType: string(c.OrderType),
		LimitPrice: c.LimitPrice, TriggerPrice: c.TriggerPrice, IdempotencyTag: c.IdempotencyTag,
		Broker: string(c.Broker), State: string(c.State), FilledQty: c.FilledQty,
		Reason: c.Reason, Created: c.Created, Updated: c.Updated,
	}
	doc.BrokerOrderID = c.BrokerOrderID
	_, err = s.db.Collection(ColOrders).ReplaceOne(ctx,
		bson.D{{Key: "id", Value: c.ID}},
		doc, options.Replace().SetUpsert(true))
	return err
}

// ListParents returns parents since the given time (any user when
// userID is empty — single-user v1).
func (s *Store) ListParents(ctx context.Context, userID string, since time.Time) ([]core.Order, error) {
	filter := bson.D{{Key: "parentid", Value: bson.D{{Key: "$exists", Value: false}}}}
	if !since.IsZero() {
		filter = append(filter, bson.E{Key: "created", Value: bson.D{{Key: "$gte", Value: since}}})
	}
	cur, err := s.db.Collection(ColOrders).Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []core.Order
	for cur.Next(ctx) {
		var d parentDoc
		if err := cur.Decode(&d); err != nil {
			continue
		}
		var inst core.Instrument
		_ = bson.Unmarshal(d.Instrument, &inst)
		out = append(out, core.Order{
			ID: d.ID, UserID: d.UserID, Instrument: inst,
			Side: core.Side(d.Side), Lots: d.Lots, TotalQty: d.TotalQty,
			OrderType: core.OrderType(d.OrderType), LimitPrice: d.LimitPrice,
			TriggerPrice: d.TriggerPrice,
			Broker:       core.Broker(d.Broker), State: core.OrderState(d.State),
			Reason: d.Reason, FilledQty: d.FilledQty, Created: d.Created, Updated: d.Updated,
		})
	}
	return out, nil
}

// ListChildren returns the slices of a parent.
func (s *Store) ListChildren(ctx context.Context, parentID string) ([]core.ChildOrder, error) {
	cur, err := s.db.Collection(ColOrders).Find(ctx, bson.D{{Key: "parentid", Value: parentID}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []core.ChildOrder
	for cur.Next(ctx) {
		var d childDoc
		if err := cur.Decode(&d); err != nil {
			continue
		}
		var inst core.Instrument
		_ = bson.Unmarshal(d.Instrument, &inst)
		out = append(out, core.ChildOrder{
			ID: d.ID, ParentID: d.ParentID, UserID: d.UserID, Instrument: inst,
			Side: core.Side(d.Side), Qty: d.Qty, OrderType: core.OrderType(d.OrderType),
			LimitPrice: d.LimitPrice, TriggerPrice: d.TriggerPrice, IdempotencyTag: d.IdempotencyTag,
			Broker: core.Broker(d.Broker), BrokerOrderID: d.BrokerOrderID,
			State: core.OrderState(d.State), FilledQty: d.FilledQty,
			Reason: d.Reason, Created: d.Created, Updated: d.Updated,
		})
	}
	return out, nil
}

// Audit appends to the audit log.
func (s *Store) Audit(ctx context.Context, entry engine.AuditEntry) error {
	doc := bson.D{
		{Key: "time", Value: entry.Time},
		{Key: "user_id", Value: entry.UserID},
		{Key: "kind", Value: entry.Kind},
		{Key: "subject", Value: entry.Subject},
		{Key: "detail", Value: entry.Detail},
	}
	_, err := s.db.Collection(ColAudit).InsertOne(ctx, doc)
	return err
}

// ---- app config (TOTP secret etc.) ----

// SaveAppConfig stores a config value.
func (s *Store) SaveAppConfig(ctx context.Context, key string, value []byte) error {
	_, err := s.db.Collection(ColConfig).ReplaceOne(ctx,
		bson.D{{Key: "_id", Value: key}},
		bson.D{{Key: "_id", Value: key}, {Key: "value", Value: string(value)}},
		options.Replace().SetUpsert(true))
	return err
}

// LoadAppConfig reads a config value.
func (s *Store) LoadAppConfig(ctx context.Context, key string) ([]byte, error) {
	var d struct {
		Value string `bson:"value"`
	}
	err := s.db.Collection(ColConfig).FindOne(ctx, bson.D{{Key: "_id", Value: key}}).Decode(&d)
	if err != nil {
		return nil, err
	}
	return []byte(d.Value), nil
}

// ---- credentials ----

type credDoc struct {
	Broker string `bson:"broker"`
	// encrypted blob: api key, api secret, refresh token, etc.
	Secret string `bson:"secret"`
}

// SaveCredentials stores a broker's credential blob encrypted.
func (s *Store) SaveCredentials(ctx context.Context, broker string, blob []byte) error {
	ct, err := s.Encrypt(blob)
	if err != nil {
		return err
	}
	_, err = s.db.Collection(ColCreds).ReplaceOne(ctx,
		bson.D{{Key: "broker", Value: broker}},
		credDoc{Broker: broker, Secret: ct},
		options.Replace().SetUpsert(true))
	return err
}

// LoadCredentials decrypts a broker's blob.
func (s *Store) LoadCredentials(ctx context.Context, broker string) ([]byte, error) {
	var d credDoc
	err := s.db.Collection(ColCreds).FindOne(ctx, bson.D{{Key: "broker", Value: broker}}).Decode(&d)
	if err != nil {
		return nil, err
	}
	return s.Decrypt(d.Secret)
}

// ---- instruments ----

type instDoc struct {
	Key          string             `bson:"_id"`
	Underlying   string             `bson:"underlying"`
	Kind         string             `bson:"kind"`
	Expiry       time.Time          `bson:"expiry"`
	Strike       float64            `bson:"strike"`
	OptionType   string             `bson:"option_type"`
	LotSize      int                `bson:"lot_size"`
	FreezeQty    int                `bson:"freeze_qty"`
	TickSize     float64            `bson:"tick_size"`
	BrokerSymbols map[string]string `bson:"broker_symbols"`
	UpdatedAt    time.Time          `bson:"updated_at"`
}

// ClearInstruments wipes the universe (sim reseeding: each restart
// mints a fresh nearest expiry; old-run instruments must not linger).
func (s *Store) ClearInstruments(ctx context.Context) error {
	_, err := s.db.Collection(ColInstruments).DeleteMany(ctx, bson.D{})
	return err
}

// SaveInstruments replaces the instrument universe (upsert per key).
func (s *Store) SaveInstruments(ctx context.Context, insts []core.Instrument) error {
	for _, i := range insts {
		d := instDoc{
			Key: i.Key(), Underlying: i.Underlying, Kind: string(i.Kind),
			Expiry: i.Expiry, Strike: i.Strike, OptionType: string(i.OptionType),
			LotSize: i.LotSize, FreezeQty: i.FreezeQty, TickSize: i.TickSize,
			BrokerSymbols: map[string]string{}, UpdatedAt: time.Now(),
		}
		for b, sym := range i.BrokerSymbols {
			d.BrokerSymbols[string(b)] = sym
		}
		_, err := s.db.Collection(ColInstruments).ReplaceOne(ctx,
			bson.D{{Key: "_id", Value: d.Key}}, d, options.Replace().SetUpsert(true))
		if err != nil {
			return err
		}
	}
	return nil
}

// LoadInstruments reads the full universe (used at login/refresh).
func (s *Store) LoadInstruments(ctx context.Context) ([]core.Instrument, error) {
	cur, err := s.db.Collection(ColInstruments).Find(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []core.Instrument
	for cur.Next(ctx) {
		var d instDoc
		if err := cur.Decode(&d); err != nil {
			continue
		}
		bs := make(map[core.Broker]string, len(d.BrokerSymbols))
		for b, sym := range d.BrokerSymbols {
			bs[core.Broker(b)] = sym
		}
		out = append(out, core.Instrument{
			Underlying: d.Underlying, Kind: core.InstrumentKind(d.Kind),
			Expiry: d.Expiry, Strike: d.Strike, OptionType: core.OptionType(d.OptionType),
			LotSize: d.LotSize, FreezeQty: d.FreezeQty, TickSize: d.TickSize,
			BrokerSymbols: bs,
		})
	}
	return out, nil
}