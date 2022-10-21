/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysql

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/dapr/components-contrib/state"
	"github.com/dapr/kit/logger"
	"github.com/dapr/kit/ptr"
)

// Optimistic Concurrency is implemented using a string column that stores
// a UUID.

const (
	// Used if the user does not configure a table name in the metadata.
	defaultTableName = "state"

	// Used if the user does not configure a database name in the metadata.
	defaultSchemaName = "dapr_state_store"

	// The key name in the metadata if the user wants a different table name
	// than the defaultTableName.
	tableNameKey = "tableName"

	// The key name in the metadata if the user wants a different database name
	// than the defaultSchemaName.
	schemaNameKey = "schemaName"

	// The key for the mandatory connection string of the metadata.
	connectionStringKey = "connectionString"

	// Standard error message if not connection string is provided.
	errMissingConnectionString = "missing connection string"

	// To connect to MySQL running in Azure over SSL you have to download a
	// SSL certificate. If this is provided the driver will connect using
	// SSL. If you have disable SSL you can leave this empty.
	// When the user provides a pem path their connection string must end with
	// &tls=custom
	// The connection string should be in the following format
	// "%s:%s@tcp(%s:3306)/%s?allowNativePasswords=true&tls=custom",'myadmin@mydemoserver', 'yourpassword', 'mydemoserver.mysql.database.azure.com', 'targetdb'.
	pemPathKey = "pemPath"
)

// MySQL state store.
type MySQL struct {
	// Name of the table to store state. If the table does not exist it will be created.
	tableName string

	// Name of the table to create to store state. If the table does not exist it will be created.
	schemaName string

	connectionString string

	// Instance of the database to issue commands to
	db *sql.DB

	features []state.Feature

	// Logger used in a functions
	logger logger.Logger

	factory iMySQLFactory
}

// NewMySQLStateStore creates a new instance of MySQL state store.
func NewMySQLStateStore(logger logger.Logger) state.Store {
	factory := newMySQLFactory(logger)

	// Store the provided logger and return the object. The rest of the
	// properties will be populated in the Init function
	return newMySQLStateStore(logger, factory)
}

// Hidden implementation for testing.
func newMySQLStateStore(logger logger.Logger, factory iMySQLFactory) *MySQL {
	// Store the provided logger and return the object. The rest of the
	// properties will be populated in the Init function
	return &MySQL{
		features: []state.Feature{state.FeatureETag, state.FeatureTransactional},
		logger:   logger,
		factory:  factory,
	}
}

// Init initializes the SQL server state store
// Implements the following interfaces:
// Store
// TransactionalStore
// Populate the rest of the MySQL object by reading the metadata and opening
// a connection to the server.
func (m *MySQL) Init(metadata state.Metadata) error {
	m.logger.Debug("Initializing MySql state store")

	val, ok := metadata.Properties[tableNameKey]
	if ok && val != "" {
		// Sanitize the table name
		if !validIdentifier(val) {
			return fmt.Errorf("table name '%s' is not valid", val)
		}
		m.tableName = val
	} else {
		// Default to the constant
		m.tableName = defaultTableName
	}

	val, ok = metadata.Properties[schemaNameKey]
	if ok && val != "" {
		// Sanitize the schema name
		if !validIdentifier(val) {
			return fmt.Errorf("schema name '%s' is not valid", val)
		}
		m.schemaName = val
	} else {
		// Default to the constant
		m.schemaName = defaultSchemaName
	}

	m.connectionString, ok = metadata.Properties[connectionStringKey]
	if !ok || m.connectionString == "" {
		m.logger.Error("Missing MySql connection string")
		return fmt.Errorf(errMissingConnectionString)
	}

	val, ok = metadata.Properties[pemPathKey]
	if ok && val != "" {
		err := m.factory.RegisterTLSConfig(val)
		if err != nil {
			m.logger.Error(err)
			return err
		}
	}

	db, err := m.factory.Open(m.connectionString)
	if err != nil {
		m.logger.Error(err)
		return err
	}

	// will be nil if everything is good or an err that needs to be returned
	return m.finishInit(db)
}

// Features returns the features available in this state store.
func (m *MySQL) Features() []state.Feature {
	return m.features
}

// Separated out to make this portion of code testable.
func (m *MySQL) finishInit(db *sql.DB) error {
	m.db = db

	err := m.ensureStateSchema()
	if err != nil {
		m.logger.Error(err)
		return err
	}

	err = m.db.Ping()
	if err != nil {
		m.logger.Error(err)
		return err
	}

	// will be nil if everything is good or an err that needs to be returned
	return m.ensureStateTable(m.tableName)
}

func (m *MySQL) ensureStateSchema() error {
	exists, err := schemaExists(m.db, m.schemaName)
	if err != nil {
		return err
	}

	if !exists {
		m.logger.Infof("Creating MySql schema '%s'", m.schemaName)
		_, err = m.db.Exec(
			fmt.Sprintf("CREATE DATABASE %s;", m.schemaName),
		)
		if err != nil {
			return err
		}
	}

	// Build a connection string that contains the new schema name
	// All MySQL connection strings must contain a / so split on it.
	parts := strings.Split(m.connectionString, "/")

	// Even if the connection string ends with a / parts will have two values
	// with the second being an empty string.
	m.connectionString = fmt.Sprintf("%s/%s%s", parts[0], m.schemaName, parts[1])

	// Close the connection we used to confirm and or create the schema
	err = m.db.Close()

	if err != nil {
		return err
	}

	// Open a connection to the new schema
	m.db, err = m.factory.Open(m.connectionString)

	return err
}

func (m *MySQL) ensureStateTable(stateTableName string) error {
	exists, err := tableExists(m.db, stateTableName)
	if err != nil {
		return err
	}

	if !exists {
		m.logger.Infof("Creating MySql state table '%s'", stateTableName)

		// updateDate is updated automactically on every UPDATE commands so you
		// never need to pass it in.
		// eTag is a UUID stored as a 36 characters string. It needs to be passed
		// in on inserts and updates and is used for Optimistic Concurrency
		// Note that stateTableName is sanitized
		//nolint:gosec
		createTable := fmt.Sprintf(`CREATE TABLE %s (
			id VARCHAR(255) NOT NULL PRIMARY KEY,
			value JSON NOT NULL,
			isbinary BOOLEAN NOT NULL,
			insertDate TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updateDate TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			eTag VARCHAR(36) NOT NULL
			);`, stateTableName)

		_, err = m.db.Exec(createTable)

		if err != nil {
			return err
		}
	}

	return nil
}

func schemaExists(db *sql.DB, schemaName string) (bool, error) {
	// Returns 1 or 0 as a string if the table exists or not
	exists := ""
	query := `SELECT EXISTS (
		SELECT SCHEMA_NAME FROM information_schema.schemata WHERE SCHEMA_NAME = ?
	) AS 'exists'`
	err := db.QueryRow(query, schemaName).Scan(&exists)
	return exists == "1", err
}

func tableExists(db *sql.DB, tableName string) (bool, error) {
	// Returns 1 or 0 as a string if the table exists or not
	exists := ""
	query := `SELECT EXISTS (
		SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_NAME = ?
	) AS 'exists'`
	err := db.QueryRow(query, tableName).Scan(&exists)
	return exists == "1", err
}

// Delete removes an entity from the store
// Store Interface.
func (m *MySQL) Delete(ctx context.Context, req *state.DeleteRequest) error {
	return state.DeleteWithOptions(ctx, m.deleteValue, req)
}

// deleteValue is an internal implementation of delete to enable passing the
// logic to state.DeleteWithRetries as a func.
func (m *MySQL) deleteValue(ctx context.Context, req *state.DeleteRequest) error {
	m.logger.Debug("Deleting state value from MySql")

	if req.Key == "" {
		return fmt.Errorf("missing key in delete operation")
	}

	var err error
	var result sql.Result

	if req.ETag == nil || *req.ETag == "" {
		result, err = m.db.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE id = ?`,
			m.tableName), req.Key)
	} else {
		result, err = m.db.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE id = ? and eTag = ?`,
			m.tableName), req.Key, *req.ETag)
	}

	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rows != 1 && req.ETag != nil && *req.ETag != "" {
		return state.NewETagError(state.ETagMismatch, nil)
	}

	return nil
}

// BulkDelete removes multiple entries from the store
// Store Interface.
func (m *MySQL) BulkDelete(ctx context.Context, req []state.DeleteRequest) error {
	m.logger.Debug("Executing BulkDelete request")

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}

	if len(req) > 0 {
		for _, d := range req {
			da := d // Fix for goSec G601: Implicit memory aliasing in for loop.
			err = m.Delete(ctx, &da)
			if err != nil {
				tx.Rollback()

				return err
			}
		}
	}

	err = tx.Commit()

	return err
}

// Get returns an entity from store
// Store Interface.
func (m *MySQL) Get(ctx context.Context, req *state.GetRequest) (*state.GetResponse, error) {
	m.logger.Debug("Getting state value from MySql")

	if req.Key == "" {
		return nil, fmt.Errorf("missing key in get operation")
	}

	var (
		eTag     string
		value    []byte
		isBinary bool
	)

	//nolint:gosec
	query := fmt.Sprintf(
		`SELECT value, eTag, isbinary FROM %s WHERE id = ?`,
		m.tableName, // m.tableName is sanitized
	)
	err := m.db.QueryRowContext(ctx, query, req.Key).Scan(&value, &eTag, &isBinary)
	if err != nil {
		// If no rows exist, return an empty response, otherwise return an error.
		if errors.Is(err, sql.ErrNoRows) {
			return &state.GetResponse{}, nil
		}

		return nil, err
	}

	if isBinary {
		var (
			s    string
			data []byte
		)

		err = json.Unmarshal(value, &s)
		if err != nil {
			return nil, err
		}

		data, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}

		return &state.GetResponse{
			Data:     data,
			ETag:     ptr.Of(eTag),
			Metadata: req.Metadata,
		}, nil
	}

	return &state.GetResponse{
		Data:     value,
		ETag:     ptr.Of(eTag),
		Metadata: req.Metadata,
	}, nil
}

// Set adds/updates an entity on store
// Store Interface.
func (m *MySQL) Set(ctx context.Context, req *state.SetRequest) error {
	return state.SetWithOptions(ctx, m.setValue, req)
}

// setValue is an internal implementation of set to enable passing the logic
// to state.SetWithRetries as a func.
func (m *MySQL) setValue(ctx context.Context, req *state.SetRequest) error {
	m.logger.Debug("Setting state value in MySql")

	err := state.CheckRequestOptions(req.Options)
	if err != nil {
		return err
	}

	if req.Key == "" {
		return errors.New("missing key in set operation")
	}

	var v any
	isBinary := false
	switch x := req.Value.(type) {
	case string:
		if x == "" {
			return errors.New("empty string is not allowed in set operation")
		}
		v = x
	case []uint8:
		isBinary = true
		v = base64.StdEncoding.EncodeToString(x)
	default:
		v = x
	}

	encB, _ := json.Marshal(v)
	enc := string(encB)
	eTag := uuid.New().String()

	var result sql.Result
	var maxRows int64 = 1

	if req.Options.Concurrency == state.FirstWrite && (req.ETag == nil || *req.ETag == "") {
		// With first-write-wins and no etag, we can insert the row only if it doesn't exist
		//nolint:gosec
		query := fmt.Sprintf(
			`INSERT INTO %s (value, id, eTag, isbinary) VALUES (?, ?, ?, ?);`,
			m.tableName, // m.tableName is sanitized
		)
		result, err = m.db.ExecContext(ctx, query, enc, req.Key, eTag, isBinary)
	} else if req.ETag != nil && *req.ETag != "" {
		// When an eTag is provided do an update - not insert
		//nolint:gosec
		query := fmt.Sprintf(
			`UPDATE %s SET value = ?, eTag = ?, isbinary = ? WHERE id = ? AND eTag = ?;`,
			m.tableName, // m.tableName is sanitized
		)
		result, err = m.db.ExecContext(ctx, query, enc, eTag, isBinary, req.Key, *req.ETag)
	} else {
		// If this is a duplicate MySQL returns that two rows affected
		maxRows = 2
		//nolint:gosec
		query := fmt.Sprintf(
			`INSERT INTO %s (value, id, eTag, isbinary) VALUES (?, ?, ?, ?) on duplicate key update value=?, eTag=?, isbinary=?;`,
			m.tableName, // m.tableName is sanitized
		)
		result, err = m.db.ExecContext(ctx, query, enc, req.Key, eTag, isBinary, enc, eTag, isBinary)
	}

	if err != nil {
		if req.ETag != nil && *req.ETag != "" {
			return state.NewETagError(state.ETagMismatch, err)
		}

		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rows == 0 {
		err = fmt.Errorf(`rows affected error: no rows match given key '%s' and eTag '%s'`, req.Key, *req.ETag)
		err = state.NewETagError(state.ETagMismatch, err)
		m.logger.Error(err)
		return err
	}

	if rows > maxRows {
		err = fmt.Errorf(`rows affected error: more than %d row affected; actual %d`, maxRows, rows)
		m.logger.Error(err)
		return err
	}

	return nil
}

// BulkSet adds/updates multiple entities on store
// Store Interface.
func (m *MySQL) BulkSet(ctx context.Context, req []state.SetRequest) error {
	m.logger.Debug("Executing BulkSet request")

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}

	if len(req) > 0 {
		for i := range req {
			err = m.Set(ctx, &req[i])
			if err != nil {
				tx.Rollback()
				return err
			}
		}
	}

	return tx.Commit()
}

// Multi handles multiple transactions.
// TransactionalStore Interface.
func (m *MySQL) Multi(ctx context.Context, request *state.TransactionalStateRequest) error {
	m.logger.Debug("Executing Multi request")

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}

	for _, req := range request.Operations {
		switch req.Operation {
		case state.Upsert:
			setReq, err := m.getSets(req)
			if err != nil {
				_ = tx.Rollback()
				return err
			}

			err = m.Set(ctx, &setReq)
			if err != nil {
				_ = tx.Rollback()
				return err
			}

		case state.Delete:
			delReq, err := m.getDeletes(req)
			if err != nil {
				_ = tx.Rollback()
				return err
			}

			err = m.Delete(ctx, &delReq)
			if err != nil {
				_ = tx.Rollback()
				return err
			}

		default:
			return fmt.Errorf("unsupported operation: %s", req.Operation)
		}
	}

	return tx.Commit()
}

// Returns the set requests.
func (m *MySQL) getSets(req state.TransactionalStateOperation) (state.SetRequest, error) {
	setReq, ok := req.Request.(state.SetRequest)
	if !ok {
		return setReq, fmt.Errorf("expecting set request")
	}

	if setReq.Key == "" {
		return setReq, fmt.Errorf("missing key in upsert operation")
	}

	return setReq, nil
}

// Returns the delete requests.
func (m *MySQL) getDeletes(req state.TransactionalStateOperation) (state.DeleteRequest, error) {
	delReq, ok := req.Request.(state.DeleteRequest)
	if !ok {
		return delReq, fmt.Errorf("expecting delete request")
	}

	if delReq.Key == "" {
		return delReq, fmt.Errorf("missing key in delete operation")
	}

	return delReq, nil
}

// BulkGet performs a bulks get operations.
func (m *MySQL) BulkGet(ctx context.Context, req []state.GetRequest) (bool, []state.BulkGetResponse, error) {
	// by default, the store doesn't support bulk get
	// return false so daprd will fallback to call get() method one by one
	return false, nil, nil
}

// Close implements io.Closer.
func (m *MySQL) Close() error {
	if m.db != nil {
		return m.db.Close()
	}

	return nil
}

// Validates an identifier, such as table or DB name.
// This is based on the rules for allowed unquoted identifiers (https://dev.mysql.com/doc/refman/8.0/en/identifiers.html), but more restrictive as it doesn't allow non-ASCII characters or the $ sign
func validIdentifier(v string) bool {
	if v == "" {
		return false
	}

	// Loop through the string as byte slice as we only care about ASCII characters
	b := []byte(v)
	for i := 0; i < len(b); i++ {
		if (b[i] >= '0' && b[i] <= '9') ||
			(b[i] >= 'a' && b[i] <= 'z') ||
			(b[i] >= 'A' && b[i] <= 'Z') ||
			b[i] == '_' {
			continue
		}
		return false
	}
	return true
}
