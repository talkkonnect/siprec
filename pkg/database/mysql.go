//go:build mysql
// +build mysql

package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
)

// MySQLConfig holds MySQL connection configuration
type MySQLConfig struct {
	Host            string
	Port            int
	Database        string
	Username        string
	Password        string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	SSLMode         string
	Charset         string
	ParseTime       bool
	Loc             string
}

// MySQLDatabase represents a MySQL database connection
type MySQLDatabase struct {
	db     *sql.DB
	config MySQLConfig
	logger *logrus.Logger
}

// NewMySQLDatabase creates a new MySQL database connection
func NewMySQLDatabase(config MySQLConfig, logger *logrus.Logger) (*MySQLDatabase, error) {
	// Build DSN (Data Source Name)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=%t&loc=%s",
		config.Username,
		config.Password,
		config.Host,
		config.Port,
		config.Database,
		config.Charset,
		config.ParseTime,
		config.Loc,
	)

	if config.SSLMode != "" {
		dsn += "&tls=" + config.SSLMode
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)
	db.SetConnMaxIdleTime(config.ConnMaxIdleTime)

	// Test the connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	mysql := &MySQLDatabase{
		db:     db,
		config: config,
		logger: logger,
	}

	logger.WithFields(logrus.Fields{
		"host":     config.Host,
		"port":     config.Port,
		"database": config.Database,
	}).Info("Connected to MySQL database")

	return mysql, nil
}

// Close closes the database connection
func (m *MySQLDatabase) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

// Health checks database health
func (m *MySQLDatabase) Health() error {
	ctx, cancel := m.getContext()
	defer cancel()

	if err := m.db.PingContext(ctx); err != nil {
		return fmt.Errorf("database health check failed: %w", err)
	}

	return nil
}

// Migrate runs database migrations
func (m *MySQLDatabase) Migrate() error {
	migrations := []string{
		createSessionsTable,
		createParticipantsTable,
		createStreamsTable,
		createCDRTable,
		createEventsTable,
		createTranscriptionsTable,
		createUsersTable,
		createAPIKeysTable,
		createSearchIndexTable,
		createSIPMessagesTable,
		createIndexes,
	}

	for i, migration := range migrations {
		m.logger.WithField("migration", i+1).Debug("Running migration")

		// A single migration string may contain multiple SQL statements.
		// The MySQL driver does not permit multiple statements in one Exec
		// unless multiStatements is enabled (which we intentionally leave
		// off), so split and execute each statement individually.
		for _, stmt := range splitSQLStatements(migration) {
			if _, err := m.db.Exec(stmt); err != nil {
				return fmt.Errorf("migration %d failed: %w", i+1, err)
			}
		}
	}

	// Post-schema fixups for databases created by older versions.
	if err := m.dropLegacyCDRSessionFK(); err != nil {
		return fmt.Errorf("dropping legacy cdr foreign key failed: %w", err)
	}

	// Add caller/callee display-name columns to CDR tables created before these
	// columns existed (CREATE TABLE IF NOT EXISTS won't alter an existing table).
	if err := m.addCDRDisplayNameColumns(); err != nil {
		return fmt.Errorf("adding cdr display-name columns failed: %w", err)
	}

	m.logger.Info("Database migrations completed successfully")
	return nil
}

// dropLegacyCDRSessionFK removes the cdr -> sessions foreign key that older
// schema versions created. The application never populates the sessions table
// in the recording path, so this FK caused every CDR insert to fail with
// MySQL error 1452. Fresh installs no longer create the FK (see
// createCDRTable); this handles databases migrated from the older schema.
// It is idempotent: if no such FK exists it is a no-op.
func (m *MySQLDatabase) dropLegacyCDRSessionFK() error {
	ctx, cancel := m.getContext()
	defer cancel()

	// Look up the actual constraint name (it may not be the default
	// "cdr_ibfk_1" depending on how the table was created).
	var constraintName string
	err := m.db.QueryRowContext(ctx, `
		SELECT CONSTRAINT_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = 'cdr'
		  AND COLUMN_NAME = 'session_id'
		  AND REFERENCED_TABLE_NAME = 'sessions'
		LIMIT 1
	`).Scan(&constraintName)
	if err == sql.ErrNoRows {
		return nil // No legacy FK present.
	}
	if err != nil {
		return fmt.Errorf("failed to inspect cdr foreign keys: %w", err)
	}

	// Constraint name comes from information_schema, not user input, but it is
	// an identifier so it cannot be parameterized; interpolate it directly.
	if _, err := m.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE cdr DROP FOREIGN KEY `%s`", constraintName)); err != nil {
		return fmt.Errorf("failed to drop cdr foreign key %q: %w", constraintName, err)
	}

	m.logger.WithField("constraint", constraintName).Info("Dropped legacy cdr->sessions foreign key")
	return nil
}

// addCDRDisplayNameColumns adds the caller_id_name and callee_id_name columns to
// the cdr table for databases created before they existed. CREATE TABLE IF NOT
// EXISTS never alters an existing table, so older installs need this explicit
// ALTER. It is idempotent: columns that already exist are skipped.
func (m *MySQLDatabase) addCDRDisplayNameColumns() error {
	ctx, cancel := m.getContext()
	defer cancel()

	columns := []struct {
		name string
		ddl  string
	}{
		{"caller_id_name", "ALTER TABLE cdr ADD COLUMN caller_id_name VARCHAR(255) NULL AFTER callee_id"},
		{"callee_id_name", "ALTER TABLE cdr ADD COLUMN callee_id_name VARCHAR(255) NULL AFTER caller_id_name"},
	}

	for _, col := range columns {
		var exists int
		err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			  AND TABLE_NAME = 'cdr'
			  AND COLUMN_NAME = ?
		`, col.name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("failed to inspect cdr column %q: %w", col.name, err)
		}
		if exists > 0 {
			continue
		}
		if _, err := m.db.ExecContext(ctx, col.ddl); err != nil {
			return fmt.Errorf("failed to add cdr column %q: %w", col.name, err)
		}
		m.logger.WithField("column", col.name).Info("Added cdr display-name column")
	}

	return nil
}

// splitSQLStatements splits a migration blob into individual executable
// statements. Full-line SQL comments ("-- ...") are stripped FIRST, then the
// remainder is split on ";" and empty fragments are dropped. Stripping
// comments before splitting is important: a ";" inside a comment must not be
// treated as a statement terminator. The schema definitions contain no
// semicolons inside string literals, so splitting the comment-free text is
// safe.
func splitSQLStatements(blob string) []string {
	// Remove full-line comments and blank lines up front.
	var kept []string
	for _, line := range strings.Split(blob, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		kept = append(kept, line)
	}

	var stmts []string
	for _, part := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt := strings.TrimSpace(part)
		if stmt != "" {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

// getContext returns a context with timeout
func (m *MySQLDatabase) getContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// Database schema definitions
const createSessionsTable = `
CREATE TABLE IF NOT EXISTS sessions (
    id VARCHAR(36) PRIMARY KEY,
    call_id VARCHAR(255) NOT NULL,
    session_id VARCHAR(255) NOT NULL,
    status ENUM('active', 'completed', 'failed', 'terminated') NOT NULL DEFAULT 'active',
    transport ENUM('udp', 'tcp', 'tls') NOT NULL,
    source_ip VARCHAR(45) NOT NULL,
    source_port INT NOT NULL,
    local_ip VARCHAR(45) NOT NULL,
    local_port INT NOT NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NULL,
    duration BIGINT NULL,
    recording_path VARCHAR(512) NOT NULL,
    metadata_xml TEXT NULL,
    sdp TEXT NULL,
    participants INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_call_id (call_id),
    INDEX idx_session_id (session_id),
    INDEX idx_status (status),
    INDEX idx_start_time (start_time),
    INDEX idx_source_ip (source_ip),
    FULLTEXT(metadata_xml)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createParticipantsTable = `
CREATE TABLE IF NOT EXISTS participants (
    id VARCHAR(36) PRIMARY KEY,
    session_id VARCHAR(36) NOT NULL,
    participant_id VARCHAR(255) NOT NULL,
    type ENUM('caller', 'callee', 'observer') NOT NULL,
    name_id VARCHAR(255) NULL,
    display_name VARCHAR(255) NULL,
    aor VARCHAR(255) NULL,
    stream_id VARCHAR(255) NULL,
    join_time TIMESTAMP NOT NULL,
    leave_time TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    INDEX idx_session_id (session_id),
    INDEX idx_participant_id (participant_id),
    INDEX idx_type (type),
    FULLTEXT(display_name, aor)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createStreamsTable = `
CREATE TABLE IF NOT EXISTS streams (
    id VARCHAR(36) PRIMARY KEY,
    session_id VARCHAR(36) NOT NULL,
    stream_id VARCHAR(255) NOT NULL,
    label VARCHAR(255) NOT NULL,
    mode ENUM('separate', 'mixed') NOT NULL DEFAULT 'separate',
    direction ENUM('sendonly', 'recvonly', 'sendrecv') NOT NULL DEFAULT 'sendonly',
    codec VARCHAR(50) NULL,
    sample_rate INT NULL,
    channels INT NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NULL,
    packet_count BIGINT NULL,
    byte_count BIGINT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    INDEX idx_session_id (session_id),
    INDEX idx_stream_id (stream_id),
    INDEX idx_label (label),
    INDEX idx_codec (codec)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createCDRTable = `
CREATE TABLE IF NOT EXISTS cdr (
    id VARCHAR(36) PRIMARY KEY,
    session_id VARCHAR(36) NOT NULL,
    call_id VARCHAR(255) NOT NULL,
    caller_id VARCHAR(255) NULL,
    callee_id VARCHAR(255) NULL,
    caller_id_name VARCHAR(255) NULL,
    callee_id_name VARCHAR(255) NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NULL,
    duration BIGINT NULL,
    recording_path VARCHAR(512) NOT NULL,
    recording_size BIGINT NULL,
    transcription_id VARCHAR(36) NULL,
    quality DECIMAL(3,2) NULL,
    transport VARCHAR(10) NOT NULL,
    source_ip VARCHAR(45) NOT NULL,
    codec VARCHAR(50) NULL,
    sample_rate INT NULL,
    participant_count INT NOT NULL DEFAULT 0,
    stream_count INT NOT NULL DEFAULT 0,
    status ENUM('completed', 'failed', 'partial') NOT NULL DEFAULT 'completed',
    error_message TEXT NULL,
    billing_code VARCHAR(100) NULL,
    cost_center VARCHAR(100) NULL,
    vendor_type VARCHAR(50) NULL,
    ucid VARCHAR(255) NULL,
    oracle_ucid VARCHAR(255) NULL,
    conversation_id VARCHAR(255) NULL,
    cisco_session_id VARCHAR(255) NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    -- NOTE: intentionally NO foreign key to sessions(id). The CDR is a
    -- self-contained call record and the recording path does not populate
    -- the sessions table, so an FK here caused every CDR insert to fail with
    -- error 1452. session_id is kept (and indexed) as a soft reference only.
    -- (Keep this comment free of semicolons -- migrations split on ';'.)
    INDEX idx_session_id (session_id),
    INDEX idx_call_id (call_id),
    INDEX idx_caller_id (caller_id),
    INDEX idx_callee_id (callee_id),
    INDEX idx_start_time (start_time),
    INDEX idx_duration (duration),
    INDEX idx_status (status),
    INDEX idx_billing_code (billing_code),
    INDEX idx_cost_center (cost_center),
    INDEX idx_vendor_type (vendor_type),
    INDEX idx_ucid (ucid),
    INDEX idx_oracle_ucid (oracle_ucid),
    INDEX idx_conversation_id (conversation_id),
    FULLTEXT(error_message)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createEventsTable = `
CREATE TABLE IF NOT EXISTS events (
    id VARCHAR(36) PRIMARY KEY,
    session_id VARCHAR(36) NULL,
    type VARCHAR(100) NOT NULL,
    level ENUM('info', 'warning', 'error', 'critical') NOT NULL DEFAULT 'info',
    message TEXT NOT NULL,
    source VARCHAR(100) NOT NULL,
    source_ip VARCHAR(45) NULL,
    user_agent VARCHAR(500) NULL,
    metadata JSON NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL,
    INDEX idx_session_id (session_id),
    INDEX idx_type (type),
    INDEX idx_level (level),
    INDEX idx_source (source),
    INDEX idx_created_at (created_at),
    FULLTEXT(message)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createTranscriptionsTable = `
CREATE TABLE IF NOT EXISTS transcriptions (
    id VARCHAR(36) PRIMARY KEY,
    session_id VARCHAR(36) NOT NULL,
    stream_id VARCHAR(255) NULL,
    provider VARCHAR(50) NOT NULL,
    language VARCHAR(10) NOT NULL DEFAULT 'en-US',
    text TEXT NOT NULL,
    confidence DECIMAL(3,2) NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NULL,
    word_count INT NOT NULL DEFAULT 0,
    speaker VARCHAR(255) NULL,
    is_final BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    INDEX idx_session_id (session_id),
    INDEX idx_stream_id (stream_id),
    INDEX idx_provider (provider),
    INDEX idx_language (language),
    INDEX idx_start_time (start_time),
    INDEX idx_is_final (is_final),
    FULLTEXT(text)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createUsersTable = `
CREATE TABLE IF NOT EXISTS users (
    id VARCHAR(36) PRIMARY KEY,
    username VARCHAR(100) UNIQUE NOT NULL,
    email VARCHAR(255) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    role ENUM('admin', 'operator', 'viewer') NOT NULL DEFAULT 'viewer',
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    last_login TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_username (username),
    INDEX idx_email (email),
    INDEX idx_role (role),
    INDEX idx_is_active (is_active)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createAPIKeysTable = `
CREATE TABLE IF NOT EXISTS api_keys (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(36) NOT NULL,
    name VARCHAR(255) NOT NULL,
    key_hash VARCHAR(255) NOT NULL,
    permissions JSON NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    expires_at TIMESTAMP NULL,
    last_used TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    INDEX idx_user_id (user_id),
    INDEX idx_key_hash (key_hash),
    INDEX idx_is_active (is_active),
    INDEX idx_expires_at (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createSearchIndexTable = `
CREATE TABLE IF NOT EXISTS search_index (
    id VARCHAR(36) PRIMARY KEY,
    type VARCHAR(50) NOT NULL,
    entity_id VARCHAR(36) NOT NULL,
    content TEXT NOT NULL,
    metadata JSON NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_type (type),
    INDEX idx_entity_id (entity_id),
    FULLTEXT(content)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createSIPMessagesTable = `
CREATE TABLE IF NOT EXISTS sip_messages (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    call_id VARCHAR(255) NOT NULL,
    session_id VARCHAR(36) NULL,
    seq INT NOT NULL DEFAULT 0,
    ts TIMESTAMP(6) NOT NULL,
    direction ENUM('recv', 'send') NOT NULL,
    method VARCHAR(32) NULL,
    status_code INT NULL,
    cseq_method VARCHAR(32) NULL,
    from_uri VARCHAR(255) NULL,
    to_uri VARCHAR(255) NULL,
    src_addr VARCHAR(64) NULL,
    dst_addr VARCHAR(64) NULL,
    raw MEDIUMTEXT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_sipmsg_call (call_id, ts, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

const createIndexes = `
-- Additional performance indexes
CREATE INDEX IF NOT EXISTS idx_sessions_composite ON sessions(status, start_time, transport);
CREATE INDEX IF NOT EXISTS idx_cdr_composite ON cdr(status, start_time, participant_count);
CREATE INDEX IF NOT EXISTS idx_events_composite ON events(level, created_at, type);
CREATE INDEX IF NOT EXISTS idx_transcriptions_composite ON transcriptions(session_id, is_final, start_time);
`
