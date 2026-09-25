// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db opens the single SQLite file that holds both signalmeow's
// protocol state (keys, sessions, recipients) and our message history.
package db

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream" // registers the sqlite3-fk-wal driver

	"go.mau.fi/mautrix-signal/pkg/signalmeow/store"
)

type DB struct {
	*dbutil.Database
	Signal *store.Container
}

// Open opens (creating if needed) the database at path and applies schema
// upgrades for signalmeow and our own tables.
func Open(ctx context.Context, path string, log zerolog.Logger) (*DB, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	raw, err := dbutil.NewWithDialect("file:"+path+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	raw.Log = dbutil.ZeroLogger(log.With().Str("db_section", "main").Logger())
	d := &DB{
		Database: raw,
		Signal:   store.NewStore(raw, dbutil.ZeroLogger(log.With().Str("db_section", "signalmeow").Logger())),
	}
	if err := d.Signal.Upgrade(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("upgrade signalmeow schema: %w", err)
	}
	if err := upgradeHistory(ctx, raw); err != nil {
		raw.Close()
		return nil, fmt.Errorf("upgrade history schema: %w", err)
	}
	return d, nil
}

// historySchema is applied in order; the index+1 is the schema version.
var historySchema = []string{
	`CREATE TABLE sh_thread (
		id          TEXT PRIMARY KEY,   -- service ID string or group identifier
		kind        TEXT NOT NULL,      -- 'direct' | 'group'
		title       TEXT NOT NULL DEFAULT '',
		last_ts     INTEGER NOT NULL DEFAULT 0,
		archived    INTEGER NOT NULL DEFAULT 0,
		muted_until INTEGER NOT NULL DEFAULT 0,
		expire_timer   INTEGER NOT NULL DEFAULT 0, -- disappearing messages, seconds
		expire_version INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE sh_message (
		id          INTEGER PRIMARY KEY,
		thread_id   TEXT NOT NULL REFERENCES sh_thread(id) ON DELETE CASCADE,
		author      TEXT NOT NULL,      -- ACI of the sender
		ts          INTEGER NOT NULL,   -- sender timestamp (ms); Signal's message identity
		server_ts   INTEGER NOT NULL DEFAULT 0,
		received_at INTEGER NOT NULL DEFAULT 0,
		outgoing    INTEGER NOT NULL DEFAULT 0,
		read        INTEGER NOT NULL DEFAULT 0, -- incoming: seen on some device
		status      TEXT NOT NULL DEFAULT '',  -- outgoing: sending|sent|delivered|read|failed
		body        TEXT NOT NULL DEFAULT '',
		quote_author TEXT NOT NULL DEFAULT '',
		quote_ts    INTEGER NOT NULL DEFAULT 0,
		quote_text  TEXT NOT NULL DEFAULT '',
		edited_at   INTEGER NOT NULL DEFAULT 0,
		deleted     INTEGER NOT NULL DEFAULT 0,
		expires_in  INTEGER NOT NULL DEFAULT 0,  -- seconds
		expire_start INTEGER NOT NULL DEFAULT 0, -- ms; timer starts when read (incoming) or sent
		sticker     TEXT NOT NULL DEFAULT '',
		UNIQUE (thread_id, author, ts)
	);
	CREATE INDEX sh_message_thread_ts ON sh_message (thread_id, ts);
	CREATE INDEX sh_message_author_ts ON sh_message (author, ts);
	CREATE INDEX sh_message_unread ON sh_message (thread_id) WHERE outgoing = 0 AND read = 0;
	CREATE TABLE sh_attachment (
		message_id   INTEGER NOT NULL REFERENCES sh_message(id) ON DELETE CASCADE,
		idx          INTEGER NOT NULL,
		content_type TEXT NOT NULL DEFAULT '',
		filename     TEXT NOT NULL DEFAULT '',
		size         INTEGER NOT NULL DEFAULT 0,
		path         TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL DEFAULT 'pending', -- pending|done|failed
		error        TEXT NOT NULL DEFAULT '',
		voice_note   INTEGER NOT NULL DEFAULT 0,
		pointer      BLOB,             -- serialized AttachmentPointer for (re)download
		PRIMARY KEY (message_id, idx)
	);
	CREATE TABLE sh_reaction (
		message_id INTEGER NOT NULL REFERENCES sh_message(id) ON DELETE CASCADE,
		reactor    TEXT NOT NULL,
		emoji      TEXT NOT NULL,
		ts         INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (message_id, reactor)
	);`,
	// v2: when a message was deleted, so its placeholder can be purged later.
	// Placeholders from before this version get the upgrade time.
	`ALTER TABLE sh_message ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0;
	UPDATE sh_message SET deleted_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000 WHERE deleted = 1;
	CREATE INDEX sh_message_deleted ON sh_message (deleted_at) WHERE deleted = 1;`,
	// v3: link previews sent with messages; their images are attachments
	// of kind 'preview'.
	`ALTER TABLE sh_attachment ADD COLUMN kind TEXT NOT NULL DEFAULT '';
	CREATE TABLE sh_preview (
		message_id  INTEGER NOT NULL REFERENCES sh_message(id) ON DELETE CASCADE,
		idx         INTEGER NOT NULL,
		url         TEXT NOT NULL,
		title       TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		date        INTEGER NOT NULL DEFAULT 0,
		image_idx   INTEGER NOT NULL DEFAULT -1, -- sh_attachment.idx of its image, or -1
		PRIMARY KEY (message_id, idx)
	);`,
}

func upgradeHistory(ctx context.Context, db *dbutil.Database) error {
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS sh_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := db.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM sh_version`).Scan(&version)
	if err != nil {
		return err
	}
	for ; version < len(historySchema); version++ {
		err := db.DoTxn(ctx, nil, func(ctx context.Context) error {
			if _, err := db.Exec(ctx, historySchema[version]); err != nil {
				return err
			}
			if _, err := db.Exec(ctx, `DELETE FROM sh_version`); err != nil {
				return err
			}
			_, err := db.Exec(ctx, `INSERT INTO sh_version (version) VALUES ($1)`, version+1)
			return err
		})
		if err != nil {
			return fmt.Errorf("history schema v%d: %w", version+1, err)
		}
	}
	return nil
}
