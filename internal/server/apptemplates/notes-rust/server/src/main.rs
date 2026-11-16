//! The notes backend in Rust: an Axum server on 127.0.0.1:$PORT that keeps its data in
//! $DATA_DIR. shartifacts strips the app's base URL, so routes start at /api/.
//!
//! Several versions of an app can run at once against the same DATA_DIR (the live one, a
//! preview, the one it just replaced), so the notes live on disk, not in memory: every
//! request reads the file, and writes hold a file lock. The file is the Go template's, so
//! an app can move between the two backends and keep its notes.

use std::{
    env, fs,
    hash::{BuildHasher, Hasher},
    io::{self, Write},
    os::unix::fs::OpenOptionsExt,
    path::{Path as FsPath, PathBuf},
    sync::Arc,
    time::{SystemTime, UNIX_EPOCH},
};

use axum::{
    Json, Router,
    body::Bytes,
    extract::{DefaultBodyLimit, Path, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{delete, get},
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::{
    net::TcpListener,
    signal::unix::{SignalKind, signal},
};

#[derive(Clone, Serialize, Deserialize)]
struct Note {
    #[serde(default)]
    id: String,
    #[serde(default)]
    text: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    author: String,
    #[serde(default)]
    created_at: String,
}

struct Notes {
    path: PathBuf,
}

/// open_private opens a file only the app's user may read, as the Go template's are.
fn open_private(path: &FsPath, truncate: bool) -> io::Result<fs::File> {
    fs::OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(truncate)
        .mode(0o600)
        .open(path)
}

impl Notes {
    /// load reads the notes: none before the first is added, and an error, never an empty
    /// list, when the file cannot be read, so a write never replaces notes it did not see.
    fn load(&self) -> Result<Vec<Note>, ApiError> {
        match fs::read(&self.path) {
            Ok(b) => Ok(serde_json::from_slice(&b)?),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(Vec::new()),
            Err(e) => Err(e.into()),
        }
    }

    /// update changes the list under a lock shared with every other process using the
    /// file. File locking blocks, so it runs off the async threads.
    async fn update<T: Send + 'static>(
        self: &Arc<Self>,
        change: impl FnOnce(&mut Vec<Note>) -> Result<T, ApiError> + Send + 'static,
    ) -> Result<T, ApiError> {
        let notes = self.clone();
        tokio::task::spawn_blocking(move || {
            let lock = open_private(&notes.path.with_extension("json.lock"), false)?;
            // Released when lock is dropped, at the end of this closure.
            lock.lock()?;
            let mut list = notes.load()?;
            let out = change(&mut list)?;
            let tmp = notes.path.with_extension("json.tmp");
            open_private(&tmp, true)?.write_all(&serde_json::to_vec_pretty(&list)?)?;
            fs::rename(&tmp, &notes.path)?;
            Ok(out)
        })
        .await?
    }
}

/// An error the API answers with, as {"error": "..."}.
struct ApiError(StatusCode, String);

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (self.0, Json(json!({ "error": self.1 }))).into_response()
    }
}

impl<E: std::error::Error> From<E> for ApiError {
    fn from(e: E) -> Self {
        ApiError(StatusCode::INTERNAL_SERVER_ERROR, e.to_string())
    }
}

/// viewer is who shartifacts says is using the app (empty for share-link visitors).
fn viewer(headers: &HeaderMap) -> String {
    headers
        .get("x-shartifacts-user")
        .and_then(|v| serde_json::from_slice::<Value>(v.as_bytes()).ok())
        .and_then(|u| u["name"].as_str().map(str::to_owned))
        .unwrap_or_default()
}

/// new_id is twelve hex digits from the standard library's randomly keyed hasher: unique
/// enough to name a note, not a secret.
fn new_id() -> String {
    let mut h = std::collections::hash_map::RandomState::new().build_hasher();
    h.write_u128(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos(),
    );
    format!("{:012x}", h.finish() & 0xffff_ffff_ffff)
}

/// now is the time in UTC as RFC 3339, which the page's Date parses.
fn now() -> String {
    let d = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let secs = d.as_secs() as i64;
    let (days, rem) = (secs.div_euclid(86_400), secs.rem_euclid(86_400));
    // Howard Hinnant's days-to-civil-date algorithm.
    let z = days + 719_468;
    let (era, doe) = (z.div_euclid(146_097), z.rem_euclid(146_097));
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{:03}Z",
        rem / 3_600,
        rem % 3_600 / 60,
        rem % 60,
        d.subsec_millis()
    )
}

async fn health() -> Json<Value> {
    Json(json!({ "ok": true, "version": env::var("APP_VERSION").unwrap_or_default() }))
}

async fn me(headers: HeaderMap) -> Json<Value> {
    let access = headers
        .get("x-shartifacts-access")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    Json(json!({ "name": viewer(&headers), "access": access }))
}

async fn list(State(notes): State<Arc<Notes>>) -> Result<Json<Vec<Note>>, ApiError> {
    Ok(Json(notes.load()?))
}

#[derive(Deserialize)]
struct NewNote {
    #[serde(default)]
    text: String,
}

async fn add(
    State(notes): State<Arc<Notes>>,
    headers: HeaderMap,
    body: Bytes,
) -> Result<(StatusCode, Json<Note>), ApiError> {
    // The body is parsed whatever its Content-Type says, as the Go template's is.
    let text = serde_json::from_slice::<NewNote>(&body)
        .map(|n| n.text.trim().to_owned())
        .unwrap_or_default();
    if text.is_empty() {
        return Err(ApiError(
            StatusCode::BAD_REQUEST,
            r#"send {"text": "…"}"#.into(),
        ));
    }
    let note = Note {
        id: new_id(),
        text,
        author: viewer(&headers),
        created_at: now(),
    };
    let added = note.clone();
    notes
        .update(move |list| {
            list.insert(0, added);
            Ok(())
        })
        .await?;
    Ok((StatusCode::CREATED, Json(note)))
}

async fn remove(
    State(notes): State<Arc<Notes>>,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    notes
        .update(move |list| {
            let i = list
                .iter()
                .position(|n| n.id == id)
                .ok_or_else(|| ApiError(StatusCode::NOT_FOUND, "no such note".into()))?;
            list.remove(i);
            Ok(())
        })
        .await?;
    Ok(Json(json!({ "deleted": true })))
}

#[tokio::main]
async fn main() -> io::Result<()> {
    let dir = env::var("DATA_DIR").unwrap_or_else(|_| ".".into());
    let notes = Arc::new(Notes {
        path: PathBuf::from(&dir).join("notes.json"),
    });

    let app = Router::new()
        .route("/api/health", get(health))
        .route("/api/me", get(me))
        .route("/api/notes", get(list).post(add))
        .route("/api/notes/{id}", delete(remove))
        .layer(DefaultBodyLimit::max(64 << 10))
        .with_state(notes);

    let host = env::var("HOST").unwrap_or_else(|_| "127.0.0.1".into());
    let port = env::var("PORT").unwrap_or_else(|_| "8081".into());
    let listener = TcpListener::bind(format!("{host}:{port}")).await?;
    println!(
        "notes listening on {}, data in {dir}",
        listener.local_addr()?
    );
    axum::serve(listener, app)
        .with_graceful_shutdown(async {
            let mut term = signal(SignalKind::terminate()).expect("a SIGTERM handler");
            tokio::select! {
                _ = term.recv() => {}
                _ = tokio::signal::ctrl_c() => {}
            }
        })
        .await
}
