package notifications

const notificationSchema = `
CREATE TABLE IF NOT EXISTS notification_settings (id INTEGER PRIMARY KEY, config BLOB NOT NULL, password TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS notification_subscriptions (
 id TEXT PRIMARY KEY, api_key TEXT NOT NULL UNIQUE, email TEXT NOT NULL, note TEXT NOT NULL,
 enabled INTEGER NOT NULL, deleted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS notification_segments (
 id INTEGER PRIMARY KEY AUTOINCREMENT, subscription TEXT NOT NULL, api_key TEXT NOT NULL,
 email TEXT NOT NULL, note TEXT NOT NULL, start INTEGER NOT NULL, until_at INTEGER NOT NULL DEFAULT 0,
 cursor INTEGER NOT NULL, schedule BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS notification_periods (
 id TEXT PRIMARY KEY, segment INTEGER NOT NULL, subscription TEXT NOT NULL, start INTEGER NOT NULL,
 end INTEGER NOT NULL, timezone TEXT NOT NULL, email TEXT NOT NULL, key_label TEXT NOT NULL, note TEXT NOT NULL,
 partial INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'open', requests INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0, unpriced INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0, retry_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
 skipped INTEGER NOT NULL DEFAULT 0, UNIQUE(segment,start));
CREATE INDEX IF NOT EXISTS idx_notification_history ON notification_periods(subscription,end);
CREATE INDEX IF NOT EXISTS idx_notification_outbox ON notification_periods(status,retry_at);
`
