// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package data

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/apid/apid-core"
	"github.com/apid/apid-core/api"
	"github.com/apid/apid-core/data/wrap"
	"github.com/apid/apid-core/logger"
	"github.com/mattn/go-sqlite3"
	"os"
	"path"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	configDataDriverKey    = "data_driver"
	configDataSourceKey    = "data_source"
	configDataPathKey      = "data_path"
	statCollectionInterval = 10
	commonDBID             = "common"
	commonDBVersion        = "base"
	defaultTraceLevel      = "warn"
)

var log, dbTraceLog apid.LogService
var config apid.ConfigService

type dbMapInfo struct {
	db     *ApidDb
	closed chan bool
}

var dbMap = make(map[string]*dbMapInfo)
var dbMapSync sync.RWMutex
var tagFieldMapper = make(map[reflect.Type]map[string]string)

type ApidDb struct {
	db    *sql.DB
	mutex *sync.Mutex
}

func (d *ApidDb) Ping() error {
	return d.db.Ping()
}

func (d *ApidDb) SetMaxIdleConns(n int) {
	d.db.SetMaxIdleConns(n)
}

func (d *ApidDb) SetMaxOpenConns(n int) {
	d.db.SetMaxOpenConns(n)
}

func (d *ApidDb) SetConnMaxLifetime(du time.Duration) {
	d.db.SetConnMaxLifetime(du)
}

func (d *ApidDb) Prepare(query string) (*sql.Stmt, error) {
	return d.db.Prepare(query)
}

func (d *ApidDb) Exec(query string, args ...interface{}) (sql.Result, error) {
	return d.db.Exec(query, args...)
}

func (d *ApidDb) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return d.db.Query(query, args...)
}

func (d *ApidDb) QueryRow(query string, args ...interface{}) *sql.Row {
	return d.db.QueryRow(query, args...)
}

func (d *ApidDb) QueryStructs(dest interface{}, query string, args ...interface{}) error {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	err = StructsFromRows(dest, rows)
	return err
}

func (d *ApidDb) Begin() (apid.Tx, error) {
	d.mutex.Lock()
	tx, err := d.db.Begin()
	if err != nil {
		d.mutex.Unlock()
		return nil, err
	}
	return &Tx{
		tx:    tx,
		mutex: d.mutex,
	}, nil
}

func (d *ApidDb) Stats() sql.DBStats {
	return d.db.Stats()
}

type Tx struct {
	tx     *sql.Tx
	mutex  *sync.Mutex
	closed bool
}

func (tx *Tx) Commit() error {
	if !tx.closed {
		defer tx.mutex.Unlock()
		tx.closed = true
	}
	return tx.tx.Commit()
}
func (tx *Tx) Exec(query string, args ...interface{}) (sql.Result, error) {
	return tx.tx.Exec(query, args...)
}
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}
func (tx *Tx) Prepare(query string) (*sql.Stmt, error) {
	return tx.tx.Prepare(query)
}
func (tx *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return tx.tx.PrepareContext(ctx, query)
}
func (tx *Tx) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return tx.tx.Query(query, args...)
}
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, query, args...)
}
func (tx *Tx) QueryRow(query string, args ...interface{}) *sql.Row {
	return tx.tx.QueryRow(query, args...)
}
func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}
func (tx *Tx) Rollback() error {
	if !tx.closed {
		defer tx.mutex.Unlock()
		tx.closed = true
	}
	return tx.tx.Rollback()
}
func (tx *Tx) Stmt(stmt *sql.Stmt) *sql.Stmt {
	return tx.tx.Stmt(stmt)
}
func (tx *Tx) StmtContext(ctx context.Context, stmt *sql.Stmt) *sql.Stmt {
	return tx.tx.StmtContext(ctx, stmt)
}

func (tx *Tx) QueryStructs(dest interface{}, query string, args ...interface{}) error {
	rows, err := tx.tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	err = StructsFromRows(dest, rows)
	return err
}

func CreateDataService() apid.DataService {
	config = apid.Config()
	log = apid.Log().ForModule("data")

	// we don't want to trace normally
	config.SetDefault("DATA_TRACE_LOG_LEVEL", defaultTraceLevel)
	dbTraceLog = apid.Log().ForModule("data_trace")

	config.SetDefault(configDataDriverKey, "sqlite3")
	config.SetDefault(configDataSourceKey, "file:%s")
	config.SetDefault(configDataPathKey, "sqlite")

	return &dataService{}
}

type dataService struct {
}

func (d *dataService) DB() (apid.DB, error) {
	return d.dbVersionForID(commonDBID, commonDBVersion)
}

func (d *dataService) DBForID(id string) (apid.DB, error) {
	if id == commonDBID {
		return nil, fmt.Errorf("reserved ID: %s", id)
	}
	return d.dbVersionForID(id, commonDBVersion)
}

func (d *dataService) DBVersion(version string) (apid.DB, error) {
	if version == commonDBVersion {
		return nil, fmt.Errorf("reserved version: %s", version)
	}
	return d.dbVersionForID(commonDBID, version)
}

func (d *dataService) DBVersionForID(id, version string) (apid.DB, error) {
	if id == commonDBID {
		return nil, fmt.Errorf("reserved ID: %s", id)
	}
	if version == commonDBVersion {
		return nil, fmt.Errorf("reserved version: %s", version)
	}
	return d.dbVersionForID(id, version)
}

// will set DB to close and delete when no more references for commonDBID, provided version
func (d *dataService) ReleaseDB(version string) {
	d.ReleaseDBForID(commonDBID, version)
}

// will set DB to close and delete when no more references for commonDBID, commonDBVersion
func (d *dataService) ReleaseCommonDB() {
	d.ReleaseDBForID(commonDBID, commonDBVersion)
}

// will set DB to close and delete when no more references for any ID
func (d *dataService) ReleaseDBForID(id, version string) {
	versionedID := VersionedDBID(id, version)

	dbMapSync.Lock()
	defer dbMapSync.Unlock()

	dbm := dbMap[versionedID]
	if dbm != nil && dbm.db != nil {
		if strings.EqualFold(config.GetString(logger.ConfigLevel), logrus.DebugLevel.String()) {
			dbm.closed <- true
		}
		log.Warn("SETTING FINALIZER")
		finalizer := Delete(versionedID)
		runtime.SetFinalizer(dbm.db, finalizer)
		dbMap[versionedID] = nil
	} else {
		log.Errorf("Cannot find DB handle for ver {%s} to release", version)
	}

	return
}

func (d *dataService) dbVersionForID(id, version string) (retDb *ApidDb, err error) {

	var stoplogchan chan bool
	versionedID := VersionedDBID(id, version)

	dbMapSync.RLock()
	dbm := dbMap[versionedID]
	dbMapSync.RUnlock()
	if dbm != nil && dbm.db != nil {
		return dbm.db, nil
	}

	dbMapSync.Lock()
	defer dbMapSync.Unlock()

	dataPath := DBPath(versionedID)

	if err = os.MkdirAll(path.Dir(dataPath), 0700); err != nil {
		return
	}

	log.Infof("LoadDB: %s", dataPath)
	source := fmt.Sprintf(config.GetString(configDataSourceKey), dataPath)
	wrappedDriverName := "dd:" + config.GetString(configDataDriverKey)
	driver := wrap.NewDriver(&sqlite3.SQLiteDriver{}, dbTraceLog)
	func() {
		// just ignore the "registered twice" panic
		defer func() {
			recover()
		}()
		sql.Register(wrappedDriverName, driver)
	}()

	db, err := sql.Open(wrappedDriverName, source)

	if err != nil {
		log.Errorf("error loading db: %s", err)
		return
	}

	retDb = &ApidDb{
		db:    db,
		mutex: &sync.Mutex{},
	}

	err = db.Ping()
	if err != nil {
		log.Errorf("error pinging db: %s", err)
		return
	}

	sqlString := "PRAGMA journal_mode=WAL;"
	_, err = db.Exec(sqlString)
	if err != nil {
		log.Errorf("error setting journal_mode: %s", err)
		return
	}

	sqlString = "PRAGMA foreign_keys = ON;"
	_, err = db.Exec(sqlString)
	if err != nil {
		log.Errorf("error enabling foreign_keys: %s", err)
		return
	}
	if strings.EqualFold(config.GetString(logger.ConfigLevel),
		logrus.DebugLevel.String()) {
		stoplogchan = logDBInfo(versionedID, db)
	}

	db.SetMaxOpenConns(config.GetInt(api.ConfigDBMaxConns))
	db.SetMaxIdleConns(config.GetInt(api.ConfigDBIdleConns))
	db.SetConnMaxLifetime(time.Duration(config.GetInt(api.ConfigDBConnsTimeout)) * time.Second)
	dbInfo := dbMapInfo{
		db:     retDb,
		closed: stoplogchan,
	}
	dbMap[versionedID] = &dbInfo
	return
}

func Delete(versionedID string) interface{} {
	return func(db *ApidDb) {
		err := db.db.Close()
		if err != nil {
			log.Errorf("error closing DB: %v", err)
		}
		dataDir := path.Dir(DBPath(versionedID))
		err = os.RemoveAll(dataDir)
		if err != nil {
			log.Errorf("error removing DB files: %v", err)
		}
		delete(dbMap, versionedID)
	}
}

func VersionedDBID(id, version string) string {
	return path.Join(id, version)
}

func DBPath(id string) string {
	storagePath := config.GetString("local_storage_path")
	relativeDataPath := config.GetString(configDataPathKey)
	return path.Join(storagePath, relativeDataPath, id, "sqlite3")
}

func logDBInfo(versionedId string, db *sql.DB) chan bool {
	stop := make(chan bool)
	go func() {
		for {
			select {
			case <-time.After(time.Duration(statCollectionInterval * time.Second)):
				log.Debugf("Current number of open DB connections for ver {%s} is {%d}",
					versionedId, db.Stats().OpenConnections)
			case <-stop:
				log.Debugf("Stop DB conn. logging for ver {%s}", versionedId)
				return
			}
		}
	}()
	return stop
}

// StructsFromRows fill the dest slice with the values of according rows.
// Each row is marshaled into a struct. The "db" tag in the struct is used for field mapping.
// It will take care of null value. Supported type mappings from Sqlite3 to Go are:
// text->string; integer->int/int64/sql.NullInt64; float->float/float64/sql.NullFloat64;
// blob->[]byte/string/sql.NullString
func StructsFromRows(dest interface{}, rows *sql.Rows) error {
	t := reflect.TypeOf(dest)
	if t == nil {
		return nil
	}
	// type of the struct
	t = t.Elem().Elem()
	//build mapper if not existent
	m, ok := tagFieldMapper[t]
	if !ok {
		m = make(map[string]string)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if key := f.Tag.Get("db"); key != "" {
				m[key] = f.Name
			}

		}
		tagFieldMapper[t] = m
	}

	colNames, err := rows.Columns()
	if err != nil {
		return err
	}
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return err
	}

	cols := make([]interface{}, len(colNames))
	slice := reflect.New(reflect.SliceOf(t)).Elem()

	for i := range cols {
		switch colTypes[i].DatabaseTypeName() {
		case "null":
			cols[i] = new(sql.NullString)
		case "text":
			cols[i] = new(sql.NullString)
		case "integer":
			cols[i] = new(sql.NullInt64)
		case "float":
			cols[i] = new(sql.NullFloat64)
		case "blob":
			cols[i] = new([]byte)
		default:
			return fmt.Errorf("unsupprted column type: %s", colTypes[i].DatabaseTypeName())
		}
	}
	for rows.Next() {
		v := reflect.New(t).Elem()
		err := rows.Scan(cols...)
		if err != nil {
			return err
		}
		for i := range cols {
			switch c := cols[i].(type) {
			case *sql.NullString:
				if f := v.FieldByName(m[colNames[i]]); f.IsValid() {
					if reflect.TypeOf(*c).AssignableTo(f.Type()) {
						f.Set(reflect.ValueOf(c).Elem())
					} else if reflect.TypeOf("").AssignableTo(f.Type()) {
						if c.Valid {
							f.SetString(c.String)
						}
					} else {
						return fmt.Errorf("cannot convert column type %s to field type %s",
							colTypes[i].DatabaseTypeName(), f.Type().String())
					}

				}
			case *sql.NullInt64:
				if f := v.FieldByName(m[colNames[i]]); f.IsValid() {
					if reflect.TypeOf(*c).AssignableTo(f.Type()) {
						f.Set(reflect.ValueOf(c).Elem())
					} else if reflect.TypeOf(int64(0)).ConvertibleTo(f.Type()) {
						if c.Valid {
							f.SetInt(c.Int64)
						}
					} else {
						return fmt.Errorf("cannot convert column type %s to field type %s",
							colTypes[i].DatabaseTypeName(), f.Type().String())
					}

				}
			case *sql.NullFloat64:
				if f := v.FieldByName(m[colNames[i]]); f.IsValid() {
					if reflect.TypeOf(*c).AssignableTo(f.Type()) {
						f.Set(reflect.ValueOf(c).Elem())
					} else if reflect.TypeOf(float64(0)).ConvertibleTo(f.Type()) {
						if c.Valid {
							f.SetFloat(c.Float64)
						}
					} else {
						return fmt.Errorf("cannot convert column type %s to field type %s",
							colTypes[i].DatabaseTypeName(), f.Type().String())
					}
				}
			case *[]byte:
				if f := v.FieldByName(m[colNames[i]]); f.IsValid() {
					if reflect.TypeOf(*c).AssignableTo(f.Type()) {
						f.SetBytes(*c)
					} else if reflect.TypeOf("").AssignableTo(f.Type()) {
						f.SetString(string(*c))
					} else if reflect.TypeOf(sql.NullString{}).AssignableTo(f.Type()) {
						f.FieldByName("String").SetString(string(*c))
						f.FieldByName("Valid").SetBool(len(*c) > 0)
					} else {
						return fmt.Errorf("cannot convert column type %s to field type %s",
							colTypes[i].DatabaseTypeName(), f.Type().String())
					}
				}
			}
		}
		slice = reflect.Append(slice, v)
	}
	reflect.ValueOf(dest).Elem().Set(slice)
	return nil
}
