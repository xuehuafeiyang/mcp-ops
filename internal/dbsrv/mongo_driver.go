package dbsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"mcp-ops/internal/config"
)

// mongoDriver 通过 JSON 描述符统一操作：
//
//	Query（读类）:
//	  {"collection":"users","op":"find","filter":{},"limit":10,"skip":0,"sort":{"_id":-1}}
//	  {"collection":"users","op":"count","filter":{}}
//	  {"collection":"users","op":"aggregate","pipeline":[...]}
//	  {"collection":"users","op":"distinct","field":"status","filter":{}}
//	  {"op":"listCollections"}  /  {"op":"dbStats"}
//
//	Execute（写类）:
//	  {"collection":"users","op":"insertOne","doc":{...}}
//	  {"collection":"users","op":"insertMany","docs":[...]}
//	  {"collection":"users","op":"updateOne","filter":{},"update":{"$set":{...}},"upsert":false}
//	  {"collection":"users","op":"updateMany","filter":{},"update":{...}}
//	  {"collection":"users","op":"deleteOne","filter":{...}}
//	  {"collection":"users","op":"deleteMany","filter":{...}}
//	  {"collection":"users","op":"createIndexes","indexes":[{"key":{"name":1},"name":"idx_name"}]}
type mongoDriver struct {
	client *mongo.Client
	db     *mongo.Database
}

func openMongo(cfg *config.Database, addr string) (*mongoDriver, error) {
	uri := "mongodb://"
	if cfg.User != "" {
		uri += url.QueryEscape(cfg.User) + ":" + url.QueryEscape(cfg.Password) + "@"
	}
	uri += addr + "/?connectTimeoutMS=10000"
	if cfg.User != "" {
		uri += "&authSource=admin"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}
	return &mongoDriver{client: client, db: client.Database(cfg.Database)}, nil
}

func (d *mongoDriver) Type() string { return "mongodb" }
func (d *mongoDriver) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = d.client.Disconnect(ctx)
}

type mongoOp struct {
	Collection string          `json:"collection"`
	Op         string          `json:"op"`
	Filter     json.RawMessage `json:"filter"`
	Doc        json.RawMessage `json:"doc"`
	Docs       json.RawMessage `json:"docs"`
	Update     json.RawMessage `json:"update"`
	Pipeline   json.RawMessage `json:"pipeline"`
	Field      string          `json:"field"`
	Limit      int64           `json:"limit"`
	Skip       int64           `json:"skip"`
	Sort       json.RawMessage `json:"sort"`
	Upsert     bool            `json:"upsert"`
	Indexes    json.RawMessage `json:"indexes"`
}

func toBSON(raw json.RawMessage, field string) (bson.D, error) {
	if len(raw) == 0 {
		return bson.D{}, nil
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON(raw, false, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return d, nil
}

func toBSONA(raw json.RawMessage, field string) (bson.A, error) {
	if len(raw) == 0 {
		return bson.A{}, nil
	}
	var a bson.A
	if err := bson.UnmarshalExtJSON(raw, false, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return a, nil
}

func (d *mongoDriver) Query(ctx context.Context, q string, _ []string) (string, error) {
	var op mongoOp
	if err := json.Unmarshal([]byte(q), &op); err != nil {
		return "", fmt.Errorf("query must be a JSON descriptor: %w", err)
	}

	switch op.Op {
	case "find":
		f, err := toBSON(op.Filter, "filter")
		if err != nil {
			return "", err
		}
		sort, err := toBSON(op.Sort, "sort")
		if err != nil {
			return "", err
		}
		opts := options.Find().SetSort(sort)
		if op.Limit > 0 {
			opts.SetLimit(op.Limit)
		}
		if op.Skip > 0 {
			opts.SetSkip(op.Skip)
		}
		cur, err := d.db.Collection(op.Collection).Find(ctx, f, opts)
		if err != nil {
			return "", err
		}
		defer cur.Close(ctx)
		var out []map[string]any
		for cur.Next(ctx) {
			var raw bson.Raw
			if err := cur.Decode(&raw); err != nil {
				return "", err
			}
			out = append(out, bsonToMap(raw))
		}
		if out == nil {
			out = []map[string]any{}
		}
		return marshalJSON(out)
	case "count":
		f, err := toBSON(op.Filter, "filter")
		if err != nil {
			return "", err
		}
		n, err := d.db.Collection(op.Collection).CountDocuments(ctx, f)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"count":%d}`, n), nil
	case "aggregate":
		p, err := toBSONA(op.Pipeline, "pipeline")
		if err != nil {
			return "", err
		}
		pipeline := make(mongo.Pipeline, 0, len(p))
		for _, st := range p {
			if d, ok := st.(bson.D); ok {
				pipeline = append(pipeline, d)
			}
		}
		cur, err := d.db.Collection(op.Collection).Aggregate(ctx, pipeline)
		if err != nil {
			return "", err
		}
		defer cur.Close(ctx)
		var out []map[string]any
		for cur.Next(ctx) {
			var raw bson.Raw
			if err := cur.Decode(&raw); err != nil {
				return "", err
			}
			out = append(out, bsonToMap(raw))
		}
		if out == nil {
			out = []map[string]any{}
		}
		return marshalJSON(out)
	case "distinct":
		f, err := toBSON(op.Filter, "filter")
		if err != nil {
			return "", err
		}
		vals, err := d.db.Collection(op.Collection).Distinct(ctx, op.Field, f)
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(vals)
		return string(out), nil
	case "listCollections":
		cur, err := d.db.ListCollections(ctx, bson.D{})
		if err != nil {
			return "", err
		}
		defer cur.Close(ctx)
		names := []string{}
		for cur.Next(ctx) {
			var v struct {
				Name string `bson:"name"`
			}
			if err := cur.Decode(&v); err == nil {
				names = append(names, v.Name)
			}
		}
		return marshalJSON(names)
	case "dbStats":
		res := d.db.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}})
		if res.Err() != nil {
			return "", res.Err()
		}
		var raw bson.Raw
		if err := res.Decode(&raw); err != nil {
			return "", err
		}
		return marshalJSON(bsonToMap(raw))
	default:
		return "", fmt.Errorf("unsupported read op %q (supported: find, count, aggregate, distinct, listCollections, dbStats)", op.Op)
	}
}

func (d *mongoDriver) Execute(ctx context.Context, q string, _ []string) (string, error) {
	var op mongoOp
	if err := json.Unmarshal([]byte(q), &op); err != nil {
		return "", fmt.Errorf("statement must be a JSON descriptor: %w", err)
	}
	coll := d.db.Collection(op.Collection)

	switch op.Op {
	case "insertOne":
		doc, err := toBSON(op.Doc, "doc")
		if err != nil {
			return "", err
		}
		res, err := coll.InsertOne(ctx, doc)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"inserted_id":%q}`, idString(res.InsertedID)), nil
	case "insertMany":
		docs, err := toBSONA(op.Docs, "docs")
		if err != nil {
			return "", err
		}
		res, err := coll.InsertMany(ctx, docs)
		if err != nil {
			return "", err
		}
		ids := make([]string, len(res.InsertedIDs))
		for i, id := range res.InsertedIDs {
			ids[i] = idString(id)
		}
		return marshalJSON(map[string]any{"inserted_count": len(ids), "inserted_ids": ids})
	case "updateOne", "updateMany":
		f, err := toBSON(op.Filter, "filter")
		if err != nil {
			return "", err
		}
		u, err := toBSON(op.Update, "update")
		if err != nil {
			return "", err
		}
		opts := options.Update().SetUpsert(op.Upsert)
		var res *mongo.UpdateResult
		if op.Op == "updateOne" {
			res, err = coll.UpdateOne(ctx, f, u, opts)
		} else {
			res, err = coll.UpdateMany(ctx, f, u, opts)
		}
		if err != nil {
			return "", err
		}
		return marshalJSON(map[string]any{
			"matched_count": res.MatchedCount, "modified_count": res.ModifiedCount, "upserted_id": idString(res.UpsertedID),
		})
	case "deleteOne", "deleteMany":
		f, err := toBSON(op.Filter, "filter")
		if err != nil {
			return "", err
		}
		var res *mongo.DeleteResult
		if op.Op == "deleteOne" {
			res, err = coll.DeleteOne(ctx, f)
		} else {
			res, err = coll.DeleteMany(ctx, f)
		}
		if err != nil {
			return "", err
		}
		return marshalJSON(map[string]any{"deleted_count": res.DeletedCount})
	case "createIndexes":
		idx, err := toBSONA(op.Indexes, "indexes")
		if err != nil {
			return "", err
		}
		var models []mongo.IndexModel
		for _, m := range idx {
			d0, _ := m.(bson.D)
			var key bson.D
			var name string
			for _, e := range d0 {
				if e.Key == "key" {
					key, _ = e.Value.(bson.D)
				}
				if e.Key == "name" {
					name, _ = e.Value.(string)
				}
			}
			models = append(models, mongo.IndexModel{Keys: key, Options: options.Index().SetName(name)})
		}
		names, err := coll.Indexes().CreateMany(ctx, models)
		if err != nil {
			return "", err
		}
		return marshalJSON(map[string]any{"created_indexes": names})
	default:
		return "", fmt.Errorf("unsupported write op %q (supported: insertOne, insertMany, updateOne, updateMany, deleteOne, deleteMany, createIndexes)", op.Op)
	}
}

func bsonToMap(raw bson.Raw) map[string]any {
	var out map[string]any
	// 先转 JSON 字节流再解出，保证 ObjectId 等类型可读
	j, err := bson.MarshalExtJSON(raw, false, false)
	if err == nil {
		_ = json.Unmarshal(j, &out)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func idString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
