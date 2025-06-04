use anyhow::Result;
use rusqlite::{params, Connection};
use std::path::Path;
use std::sync::Arc;
use tokio::sync::Mutex;

#[derive(Clone, Debug)] // Add Clone and Debug derivation
pub struct StorageManager {
    conn: Arc<Mutex<Connection>>,
}

impl StorageManager {
    pub async fn new(data_dir: &Path) -> Result<Self> {
        // Ensure data directory exists
        tokio::fs::create_dir_all(data_dir).await?;

        let db_path = data_dir.join("hivemind.db");

        // Open connection in a blocking task since SQLite operations are blocking
        let conn = tokio::task::spawn_blocking(move || -> Result<Connection> {
            let conn = Connection::open(db_path)?;

            // Create tables if they don't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS nodes (
                    id TEXT PRIMARY KEY,
                    address TEXT NOT NULL,
                    last_seen INTEGER NOT NULL
                )",
                [],
            )?;

            conn.execute(
                "CREATE TABLE IF NOT EXISTS containers (
                    id TEXT PRIMARY KEY,
                    name TEXT NOT NULL,
                    image TEXT NOT NULL,
                    status TEXT NOT NULL,
                    node_id TEXT NOT NULL
                )",
                [],
            )?;

            Ok(conn)
        })
        .await??;

        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    pub async fn save_container(
        &self,
        id: &str,
        name: &str,
        image: &str,
        status: &str,
        node_id: &str,
    ) -> Result<()> {
        let id = id.to_string();
        let name = name.to_string();
        let image = image.to_string();
        let status = status.to_string();
        let node_id = node_id.to_string();

        let conn = self.conn.clone();

        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();

            conn.execute(
                "INSERT OR REPLACE INTO containers (id, name, image, status, node_id) VALUES (?, ?, ?, ?, ?)",
                params![id, name, image, status, node_id],
            )?;

            Ok(())
        })
        .await??;

        Ok(())
    }

    pub async fn get_containers(&self) -> Result<Vec<(String, String, String, String, String)>> {
        let conn = self.conn.clone();

        let results = tokio::task::spawn_blocking(
            move || -> Result<Vec<(String, String, String, String, String)>> {
                let conn = conn.blocking_lock();
                let mut stmt =
                    conn.prepare("SELECT id, name, image, status, node_id FROM containers")?;
                let container_iter = stmt.query_map([], |row| {
                    Ok((
                        row.get(0)?,
                        row.get(1)?,
                        row.get(2)?,
                        row.get(3)?,
                        row.get(4)?,
                    ))
                })?;

                let mut containers = Vec::new();
                for container in container_iter {
                    containers.push(container?);
                }
                Ok(containers)
            },
        )
        .await??;

        Ok(results)
    }

    // Add methods to retrieve stored data
    pub async fn get_nodes(&self) -> Result<Vec<(String, String, i64)>> {
        let conn = self.conn.clone();

        let results = tokio::task::spawn_blocking(move || -> Result<Vec<(String, String, i64)>> {
            let conn = conn.blocking_lock();
            let mut stmt = conn.prepare("SELECT id, address, last_seen FROM nodes")?;
            let node_iter =
                stmt.query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))?;

            let mut nodes = Vec::new();
            for node in node_iter {
                nodes.push(node?);
            }
            Ok(nodes)
        })
        .await??;

        Ok(results)
    }

    pub async fn save_node(&self, id: &str, address: &str, last_seen: i64) -> Result<()> {
        let id = id.to_string();
        let address = address.to_string();
        let conn = self.conn.clone();

        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();

            conn.execute(
                "INSERT OR REPLACE INTO nodes (id, address, last_seen) VALUES (?, ?, ?)",
                params![id, address, last_seen],
            )?;

            Ok(())
        })
        .await??;

        Ok(())
    }

    // Generic key-value storage methods for testing
    pub async fn store(&self, key: &str, data: &[u8]) -> Result<()> {
        let key = key.to_string();
        let data = data.to_vec();
        let conn = self.conn.clone();

        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();

            // Create key-value table if it doesn't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS key_value (
                    key TEXT PRIMARY KEY,
                    value BLOB NOT NULL
                )",
                [],
            )?;

            conn.execute(
                "INSERT OR REPLACE INTO key_value (key, value) VALUES (?, ?)",
                params![key, data],
            )?;

            Ok(())
        })
        .await??;

        Ok(())
    }

    pub async fn get(&self, key: &str) -> Result<Option<Vec<u8>>> {
        let key = key.to_string();
        let conn = self.conn.clone();

        let result = tokio::task::spawn_blocking(move || -> Result<Option<Vec<u8>>> {
            let conn = conn.blocking_lock();

            // Create key-value table if it doesn't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS key_value (
                    key TEXT PRIMARY KEY,
                    value BLOB NOT NULL
                )",
                [],
            )?;

            let mut stmt = conn.prepare("SELECT value FROM key_value WHERE key = ?")?;
            let mut rows = stmt.query_map(params![key], |row| {
                let data: Vec<u8> = row.get(0)?;
                Ok(data)
            })?;

            if let Some(row) = rows.next() {
                Ok(Some(row?))
            } else {
                Ok(None)
            }
        })
        .await??;

        Ok(result)
    }
}
