package mariadb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-mysql-org/go-mysql/schema"
	_ "github.com/go-sql-driver/mysql"
	"github.com/retail-ai-inc/sync/pkg/config"
	"github.com/sirupsen/logrus"
)

// MariaDBSyncer is the structure for MariaDB synchronization
type MariaDBSyncer struct {
	cfg    config.SyncConfig
	logger *logrus.Logger
}

func NewMariaDBSyncer(cfg config.SyncConfig, logger *logrus.Logger) *MariaDBSyncer {
	return &MariaDBSyncer{
		cfg:    cfg,
		logger: logger,
	}
}

// Start function: start the synchronization process
func (s *MariaDBSyncer) Start(ctx context.Context) {
	// 1. Create canal configuration
	cfg := canal.NewDefaultConfig()
	cfg.Addr = s.parseAddr(s.cfg.SourceConnection)
	cfg.User, cfg.Password = s.parseUserPassword(s.cfg.SourceConnection)
	cfg.Dump.ExecutionPath = s.cfg.DumpExecutionPath

	// 2. Only include the tables we need
	includeTables := []string{}
	for _, mapping := range s.cfg.Mappings {
		for _, table := range mapping.Tables {
			includeTables = append(includeTables, fmt.Sprintf("%s\\.%s", mapping.SourceDatabase, table.SourceTable))
		}
	}
	cfg.IncludeTableRegex = includeTables

	// 3. Create canal instance
	c, err := canal.NewCanal(cfg)
	if err != nil {
		s.logger.Fatalf("Failed to create canal for MariaDB: %v", err)
	}

	// 4. Initialize target database connection
	targetDB, err := sql.Open("mysql", s.cfg.TargetConnection)
	if err != nil {
		s.logger.Fatalf("Failed to connect to target MariaDB database: %v", err)
	}
	// Decide if you need defer targetDB.Close() based on your usage

	// 5. Perform initial full sync if the target table is empty
	s.doInitialFullSyncIfNeeded(ctx, c, targetDB)

	// 6. Set EventHandler for incremental sync
	h := &MariaDBEventHandler{
		targetDB:          targetDB,
		mappings:          s.cfg.Mappings,
		logger:            s.logger,
		positionSaverPath: s.cfg.MySQLPositionPath,
		canal:             c,
	}
	c.SetEventHandler(h)

	// 7. Ensure the binlog position file directory exists
	if s.cfg.MySQLPositionPath != "" {
		positionDir := filepath.Dir(s.cfg.MySQLPositionPath)
		if err := os.MkdirAll(positionDir, os.ModePerm); err != nil {
			s.logger.Fatalf("Failed to create directory for MariaDB position file %s: %v", s.cfg.MySQLPositionPath, err)
		}
	}

	// 8. If binlog position was previously saved, load it
	var startPos *mysql.Position
	if s.cfg.MySQLPositionPath != "" {
		startPos = s.loadBinlogPosition(s.cfg.MySQLPositionPath)
		if startPos != nil {
			s.logger.Infof("Starting MariaDB canal from saved position: %v", *startPos)
		}
	}

	// 9. Start a goroutine to periodically save the binlog position
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pos := c.SyncedPosition()
				data, err := json.Marshal(pos)
				if err != nil {
					s.logger.Errorf("Failed to marshal binlog position: %v", err)
					continue
				}
				if h.positionSaverPath != "" {
					positionDir := filepath.Dir(h.positionSaverPath)
					if err := os.MkdirAll(positionDir, os.ModePerm); err != nil {
						s.logger.Errorf("Failed to create directory for MariaDB position file %s: %v", h.positionSaverPath, err)
						continue
					}
					if err := ioutil.WriteFile(h.positionSaverPath, data, 0644); err != nil {
						s.logger.Errorf("Failed to write binlog position to %s: %v", h.positionSaverPath, err)
					}
				}
			}
		}
	}()

	// 10. Run canal for incremental sync
	go func() {
		if startPos != nil {
			err = c.RunFrom(*startPos)
		} else {
			err = c.Run()
		}
		if err != nil {
			s.logger.Fatalf("Failed to run canal for MariaDB: %v", err)
		}
	}()

	// 11. Wait for context to end
	<-ctx.Done()
	s.logger.Info("MariaDB synchronization stopped.")
}

// Perform initial full sync if needed (batch insertion)
func (s *MariaDBSyncer) doInitialFullSyncIfNeeded(ctx context.Context, c *canal.Canal, targetDB *sql.DB) {
	// Reconnect to the source DB with the same DSN to manually query
	sourceDB, err := sql.Open("mysql", s.cfg.SourceConnection)
	if err != nil {
		s.logger.Fatalf("Failed to open source DB for initial sync in MariaDB: %v", err)
	}
	defer sourceDB.Close()

	const batchSize = 100

	for _, mapping := range s.cfg.Mappings {
		sourceDBName := mapping.SourceDatabase
		targetDBName := mapping.TargetDatabase

		for _, tableMap := range mapping.Tables {
			// 1) Check if the target table is empty
			targetCountQuery := fmt.Sprintf("SELECT COUNT(1) FROM %s.%s", targetDBName, tableMap.TargetTable)
			var count int
			if err := targetDB.QueryRow(targetCountQuery).Scan(&count); err != nil {
				s.logger.Errorf("[MariaDB] Could not check if target table %s.%s is empty: %v",
					targetDBName, tableMap.TargetTable, err)
				continue
			}

			if count > 0 {
				s.logger.Infof("[MariaDB] Target table %s.%s already has %d rows. Skip initial sync.",
					targetDBName, tableMap.TargetTable, count)
				continue
			}

			s.logger.Infof("[MariaDB] Target table %s.%s is empty. Doing initial full sync from source %s.%s...",
				targetDBName, tableMap.TargetTable, sourceDBName, tableMap.SourceTable)

			// 2) Get source table columns
			cols, err := s.getColumnsOfTable(ctx, sourceDB, sourceDBName, tableMap.SourceTable)
			if err != nil {
				s.logger.Errorf("[MariaDB] Failed to get columns of source table %s.%s: %v",
					sourceDBName, tableMap.SourceTable, err)
				continue
			}

			// 3) Read data from source table
			selectSQL := fmt.Sprintf("SELECT %s FROM %s.%s", strings.Join(cols, ","), sourceDBName, tableMap.SourceTable)
			srcRows, err := sourceDB.QueryContext(ctx, selectSQL)
			if err != nil {
				s.logger.Errorf("[MariaDB] Failed to query source table %s.%s: %v",
					sourceDBName, tableMap.SourceTable, err)
				continue
			}

			insertedCount := 0
			batchRows := make([][]interface{}, 0, batchSize)

			// Batch read
			for srcRows.Next() {
				rowValues := make([]interface{}, len(cols))
				valuePtrs := make([]interface{}, len(cols))
				for i := range cols {
					valuePtrs[i] = &rowValues[i]
				}
				if err := srcRows.Scan(valuePtrs...); err != nil {
					s.logger.Errorf("[MariaDB] Failed to scan row from %s.%s: %v",
						sourceDBName, tableMap.SourceTable, err)
					continue
				}

				batchRows = append(batchRows, rowValues)
				if len(batchRows) == batchSize {
					// Batch insert
					err := s.batchInsert(ctx, targetDB, targetDBName, tableMap.TargetTable, cols, batchRows)
					if err != nil {
						s.logger.Errorf("[MariaDB] Batch insert failed: %v", err)
					} else {
						insertedCount += len(batchRows)
					}
					batchRows = batchRows[:0]
				}
			}
			srcRows.Close()

			// Process remaining rows
			if len(batchRows) > 0 {
				err := s.batchInsert(ctx, targetDB, targetDBName, tableMap.TargetTable, cols, batchRows)
				if err != nil {
					s.logger.Errorf("[MariaDB] Last batch insert failed: %v", err)
				} else {
					insertedCount += len(batchRows)
				}
			}

			s.logger.Infof("[MariaDB] Initial sync for %s.%s -> %s.%s completed. Inserted %d rows.",
				sourceDBName, tableMap.SourceTable, targetDBName, tableMap.TargetTable, insertedCount)
		}
	}
}

// batchInsert: insert multiple rows at once
func (s *MariaDBSyncer) batchInsert(
	ctx context.Context,
	db *sql.DB,
	dbName, tableName string,
	cols []string,
	rows [][]interface{},
) error {
	if len(rows) == 0 {
		return nil
	}

	insertSQL := fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES",
		dbName,
		tableName,
		strings.Join(cols, ", "))

	singleRowPlaceholder := fmt.Sprintf("(%s)", strings.Join(makeQuestionMarks(len(cols)), ","))
	var allPlaceholder []string
	for range rows {
		allPlaceholder = append(allPlaceholder, singleRowPlaceholder)
	}
	insertSQL = insertSQL + " " + strings.Join(allPlaceholder, ", ")

	var args []interface{}
	for _, rowData := range rows {
		args = append(args, rowData...)
	}

	_, err := db.ExecContext(ctx, insertSQL, args...)
	if err != nil {
		return fmt.Errorf("batchInsert Exec failed: %w", err)
	}
	return nil
}

// getColumnsOfTable uses SHOW COLUMNS to get table columns (allowing default etc. to be NULL)
func (s *MariaDBSyncer) getColumnsOfTable(ctx context.Context, db *sql.DB, database, table string) ([]string, error) {
	query := fmt.Sprintf("SHOW COLUMNS FROM %s.%s", database, table)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var field, typeStr, nullStr, keyStr, defaultStr, extraStr sql.NullString

		if err := rows.Scan(&field, &typeStr, &nullStr, &keyStr, &defaultStr, &extraStr); err != nil {
			return nil, fmt.Errorf("failed to scan columns info from table %s.%s: %v", database, table, err)
		}
		if field.Valid {
			cols = append(cols, field.String)
		} else {
			return nil, fmt.Errorf("invalid column name for table %s.%s", database, table)
		}
	}
	return cols, nil
}

func makeQuestionMarks(n int) []string {
	res := make([]string, n)
	for i := 0; i < n; i++ {
		res[i] = "?"
	}
	return res
}

// loadBinlogPosition reads the binlog position
func (s *MariaDBSyncer) loadBinlogPosition(path string) *mysql.Position {
	positionDir := filepath.Dir(path)
	if err := os.MkdirAll(positionDir, os.ModePerm); err != nil {
		s.logger.Errorf("Failed to create directory for MariaDB position file %s: %v", path, err)
		return nil
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		s.logger.Infof("No previous binlog position file at %s: %v", path, err)
		return nil
	}
	if len(data) <= 1 {
		s.logger.Infof("Binlog position file for %s is empty", path)
		return nil
	}
	var pos mysql.Position
	if err := json.Unmarshal(data, &pos); err != nil {
		s.logger.Errorf("Failed to unmarshal binlog position from %s: %v", path, err)
		return nil
	}
	return &pos
}

// parseAddr from DSN
func (s *MariaDBSyncer) parseAddr(dsn string) string {
	parts := strings.Split(dsn, "@tcp(")
	if len(parts) < 2 {
		s.logger.Fatalf("Invalid DSN format for MariaDB: %s", dsn)
	}
	addr := strings.Split(parts[1], ")")[0]
	return addr
}

// parseUserPassword from DSN
func (s *MariaDBSyncer) parseUserPassword(dsn string) (string, string) {
	parts := strings.Split(dsn, "@")
	if len(parts) < 2 {
		s.logger.Fatalf("Invalid DSN format for MariaDB: %s", dsn)
	}
	userInfo := parts[0]
	userParts := strings.Split(userInfo, ":")
	if len(userParts) < 2 {
		s.logger.Fatalf("Invalid DSN user info for MariaDB: %s", userInfo)
	}
	return userParts[0], userParts[1]
}

// ------------------ Incremental sync event handler ------------------

type MariaDBEventHandler struct {
	canal.DummyEventHandler
	targetDB          *sql.DB
	mappings          []config.DatabaseMapping
	logger            *logrus.Logger
	positionSaverPath string
	canal             *canal.Canal
}

// OnRow handles binlog row events
func (h *MariaDBEventHandler) OnRow(e *canal.RowsEvent) error {
	table := e.Table
	sourceDB := table.Schema
	tableName := table.Name

	var targetDBName, targetTableName string
	found := false

	for _, mapping := range h.mappings {
		if mapping.SourceDatabase == sourceDB {
			for _, tableMap := range mapping.Tables {
				if tableMap.SourceTable == tableName {
					targetDBName = mapping.TargetDatabase
					targetTableName = tableMap.TargetTable
					found = true
					break
				}
			}
		}
		if found {
			break
		}
	}

	if !found {
		h.logger.Warnf("No mapping found for source table %s.%s (MariaDB)", sourceDB, tableName)
		return nil
	}

	columnNames := make([]string, len(table.Columns))
	for i, col := range table.Columns {
		columnNames[i] = col.Name
	}

	switch e.Action {
	case canal.InsertAction:
		for _, row := range e.Rows {
			h.handleInsert(targetDBName, targetTableName, columnNames, row)
		}
	case canal.UpdateAction:
		for i := 0; i < len(e.Rows); i += 2 {
			oldRow := e.Rows[i]
			newRow := e.Rows[i+1]
			h.handleUpdate(targetDBName, targetTableName, columnNames, table, oldRow, newRow)
		}
	case canal.DeleteAction:
		for _, row := range e.Rows {
			h.handleDelete(targetDBName, targetTableName, columnNames, table, row)
		}
	}
	return nil
}

// handleInsert for insert events
func (h *MariaDBEventHandler) handleInsert(targetDBName, targetTableName string, columnNames []string, row []interface{}) {
	placeholders := make([]string, len(columnNames))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	query := fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES (%s)",
		targetDBName, targetTableName,
		strings.Join(columnNames, ", "),
		strings.Join(placeholders, ", "))

	_, err := h.targetDB.Exec(query, row...)
	if err != nil {
		h.logger.Errorf("[MariaDB] Failed to insert into target database: %v", err)
	}
}

// handleUpdate for update events
func (h *MariaDBEventHandler) handleUpdate(
	targetDBName, targetTableName string,
	columnNames []string,
	table *schema.Table,
	oldRow, newRow []interface{},
) {
	setClauses := make([]string, len(columnNames))
	for i, col := range columnNames {
		setClauses[i] = fmt.Sprintf("%s = ?", col)
	}
	var whereClauses []string
	var whereValues []interface{}

	// Use primary key as WHERE condition
	for _, pkIndex := range table.PKColumns {
		whereClauses = append(whereClauses, fmt.Sprintf("%s = ?", columnNames[pkIndex]))
		whereValues = append(whereValues, oldRow[pkIndex])
	}
	if len(whereClauses) == 0 {
		h.logger.Warnf("[MariaDB] No primary key defined on table %s.%s, cannot perform update",
			targetDBName, targetTableName)
		return
	}

	query := fmt.Sprintf("UPDATE %s.%s SET %s WHERE %s",
		targetDBName, targetTableName,
		strings.Join(setClauses, ", "),
		strings.Join(whereClauses, " AND "))

	args := append(newRow, whereValues...)
	_, err := h.targetDB.Exec(query, args...)
	if err != nil {
		h.logger.Errorf("[MariaDB] Failed to update target database: %v", err)
	}
}

// handleDelete for delete events
func (h *MariaDBEventHandler) handleDelete(
	targetDBName, targetTableName string,
	columnNames []string,
	table *schema.Table,
	row []interface{},
) {
	var whereClauses []string
	var whereValues []interface{}

	for _, pkIndex := range table.PKColumns {
		whereClauses = append(whereClauses, fmt.Sprintf("%s = ?", columnNames[pkIndex]))
		whereValues = append(whereValues, row[pkIndex])
	}
	if len(whereClauses) == 0 {
		h.logger.Warnf("[MariaDB] No primary key defined on table %s.%s, cannot perform delete",
			targetDBName, targetTableName)
		return
	}

	query := fmt.Sprintf("DELETE FROM %s.%s WHERE %s",
		targetDBName,
		targetTableName,
		strings.Join(whereClauses, " AND "))
	_, err := h.targetDB.Exec(query, whereValues...)
	if err != nil {
		h.logger.Errorf("[MariaDB] Failed to delete from target database: %v", err)
	}
}

// String identifies the event handler
func (h *MariaDBEventHandler) String() string {
	return "MariaDBEventHandler"
}

// OnPosSynced does not write positions here; modify if needed
func (h *MariaDBEventHandler) OnPosSynced(header *replication.EventHeader, pos mysql.Position, gs mysql.GTIDSet, force bool) error {
	return nil
}
