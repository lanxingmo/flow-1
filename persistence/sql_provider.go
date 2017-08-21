package persistence

import (
	"github.com/golang/protobuf/proto"
	"net/url"
	"fmt"
	"path/filepath"
	"os"
	"github.com/sirupsen/logrus"
	"database/sql"
	"github.com/jmoiron/sqlx"
	"strings"
	"reflect"
	"github.com/AsynkronIT/protoactor-go/persistence"
	"github.com/fnproject/completer/setup"
	"strconv"
)

type SqlProvider struct {
	snapshotInterval int
	db               *sqlx.DB
}

var tables = [...]string{`CREATE TABLE IF NOT EXISTS events (
	actor_name varchar(255) NOT NULL,
	event_type varchar(255) NOT NULL,
	event_index int NOT NULL,
	event BLOB NOT NULL);`,

	`CREATE TABLE IF NOT EXISTS snapshots (
	actor_name varchar(255) NOT NULL PRIMARY KEY ,
	snapshot_type varchar(255) NOT NULL,
	event_index int NOT NULL,
	snapshot BLOB NOT NULL);`,
}

var log = logrus.New().WithField("logger", "persistence")

func NewSqlProvider(url *url.URL, snapshotInterval int) (*SqlProvider, error) {

	driver := url.Scheme
	switch driver {
	case "mysql", "sqlite3":
	default:

		return nil, fmt.Errorf("Invalid db driver %s", driver)
	}

	if driver == "sqlite3" {
		dir := filepath.Dir(url.Path)
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return nil, err
		}
	}
	var uri = url.String()

	uri = strings.TrimPrefix(url.String(), url.Scheme+"://")

	sqldb, err := sql.Open(driver, uri)
	if err != nil {
		logrus.WithFields(logrus.Fields{"url": uri}).WithError(err).Error("couldn't open db")
		return nil, err
	}

	sqlxDb := sqlx.NewDb(sqldb, driver)
	err = sqlxDb.Ping()
	if err != nil {
		logrus.WithFields(logrus.Fields{"url": uri}).WithError(err).Error("couldn't ping db")
		return nil, err
	}

	maxIdleConns := 256 // TODO we need to strip this out of the URL probably
	sqlxDb.SetMaxIdleConns(maxIdleConns)
	switch driver {
	case "sqlite3":
		sqlxDb.SetMaxOpenConns(1)
	}
	for _, v := range tables {
		_, err = sqlxDb.Exec(v)
		if err != nil {
			return nil, fmt.Errorf("Failed to create database table %s: %v", v, err)
		}
	}

	log.WithField("db_url", url.String()).Info("Created SQL persistence provider")
	return &SqlProvider{
		snapshotInterval: snapshotInterval,
		db:               sqlxDb,
	}, nil
}

func (provider *SqlProvider) Restart() {}

func (provider *SqlProvider) GetSnapshotInterval() int {
	return provider.snapshotInterval
}

func (provider *SqlProvider) GetSnapshot(actorName string) (snapshot interface{}, eventIndex int, ok bool) {

	row := provider.db.QueryRowx("SELECT snapshot_type,event_index,snapshot FROM snapshots WHERE actor_name = ?", actorName)

	if row.Err() != nil {
		log.WithField("actor_name", actorName).Errorf("Error getting snapshot value from DB ", row.Err())
		return nil, -1, false
	}

	var snapshotType string
	var snapshotBytes []byte

	err := row.Scan(&snapshotType, &eventIndex, &snapshotBytes)
	if err == sql.ErrNoRows {
		return nil, -1, false
	}

	if err != nil {
		log.WithField("actor_name", actorName).Errorf("Error snapshot value from DB ", err)
		return nil, -1, false
	}
	message, err := extractData(actorName, snapshotType, snapshotBytes)

	if err != nil {
		log.WithFields(logrus.Fields{"actor_name": actorName, "message_type": snapshotType}).WithError(err).Errorf("Failed to read  protobuf for snapshot")
		return nil, -1, false
	}

	return message, eventIndex, true
}

func extractData(actorName string, msgTypeName string, msgBytes []byte) (proto.Message, error) {
	protoType := proto.MessageType(msgTypeName)

	if protoType == nil {
		log.WithFields(logrus.Fields{"actor_name": actorName, "message_type": msgTypeName}).Errorf("protocol type not supported by protobuf")
		return nil, fmt.Errorf("Unsupported protocol type %s", protoType)
	}
	t := protoType.Elem()
	intPtr := reflect.New(t)
	message := intPtr.Interface().(proto.Message)

	err := proto.Unmarshal(msgBytes, message)
	if err != nil {
		return nil, err
	}
	return message, nil
}

func (provider *SqlProvider) PersistSnapshot(actorName string, eventIndex int, snapshot proto.Message) {
	pbType := proto.MessageName(snapshot)
	pbBytes, err := proto.Marshal(snapshot)

	if err != nil {
		panic(err)
	}

	_, err = provider.db.Exec("INSERT OR REPLACE INTO snapshots (actor_name,snapshot_type,event_index,snapshot) VALUES (?,?,?,?)",
		actorName, pbType, eventIndex, pbBytes)

	if err != nil {
		panic(err)
	}
}

func (provider *SqlProvider) GetEvents(actorName string, eventIndexStart int, callback func(e interface{})) {
	rows, err := provider.db.Queryx("SELECT event_type,event_index,event FROM events where actor_name = ? AND event_index >= ? ORDER BY event_index ASC", actorName, eventIndexStart)
	if err != nil {
		log.WithField("actor_name", actorName).WithError(err).Error("Error getting events value from DB ")

		// DON't PANIC ?
		panic(err)
	}
	defer rows.Close()

	for rows.Next() {
		var eventType string
		var eventIndex int
		var eventBytes []byte
		rows.Scan(&eventType, &eventIndex, &eventBytes)

		msg, err := extractData(actorName, eventType, eventBytes)
		if err != nil {
			panic(err)
		}
		callback(msg)
	}

}

func (provider *SqlProvider) PersistEvent(actorName string, eventIndex int, event proto.Message) {
	pbType := proto.MessageName(event)
	pbBytes, err := proto.Marshal(event)

	if err != nil {
		panic(err)
	}

	_, err = provider.db.Exec("INSERT OR REPLACE INTO events (actor_name,event_type,event_index,event) VALUES (?,?,?,?)",
		actorName, pbType, eventIndex, pbBytes)

	if err != nil {
		panic(err)
	}
}

func NewProviderFromEnv() (persistence.ProviderState, error) {
	dbUrlString := setup.GetString(setup.EnvDBURL)
	dbUrl, err := url.Parse(dbUrlString)
	if err != nil {
		return nil, fmt.Errorf("Invalid DB URL in %s : %s", setup.EnvDBURL, dbUrlString)
	}

	snapshotIntervalStr := setup.GetString(setup.EnvSnapshotInterval)
	snapshotInterval, ok := strconv.Atoi(snapshotIntervalStr)
	if ok != nil {
		snapshotInterval = 1000
	}
	if dbUrl.Scheme == "inmem" {
		log.Info("Using in-memory persistence")
		return persistence.NewInMemoryProvider(snapshotInterval), nil
	}
	return NewSqlProvider(dbUrl, snapshotInterval)
}