//! 性能探针（2026-09-30 审查 C 类）。默认忽略，手动运行：
//!
//! ```bash
//! cargo test --release --config 'profile.release.opt-level=3' --config 'profile.release.lto=false' \
//!   --config 'profile.release.codegen-units=16' --lib perf_ -- --ignored --nocapture --test-threads=1
//! ```
//!
//! 数据量级沿用审查报告；只打印数字，不做断言（断言放在各模块的功能测试里）。

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use super::secure::Incoming;
use super::{stub, test_inner, test_inner_at, Inner};
use crate::content::{Content, ContentBody, WireAttachment};
use crate::store::{MsgRow, StoredAttachment, StoredBody, StoredMessage};
use crate::types::*;

fn row(conv: &str, id: String, ts: i64, read: bool, body: StoredBody) -> MsgRow {
    MsgRow {
        id,
        conv_id: conv.into(),
        sender_id: "bob".into(),
        outgoing: false,
        sent_at: ts,
        server_ts: ts,
        sort_ts: ts,
        status: if read { MessageStatus::Read } else { MessageStatus::Delivered },
        msg: StoredMessage::plain(body, None),
        expire_secs: 0,
        expires_at: None,
        deleted: false,
        read,
        mentions_me: false,
    }
}

fn text(i: usize) -> StoredBody {
    StoredBody::Text { text: format!("第 {i} 条普通消息，内容是一些中文与 english words 混排的文本") }
}

fn fill(inner: &Inner, conv: &str, n: usize, read: bool, body: impl Fn(usize) -> StoredBody) {
    inner.store.ensure_conv(conv, 0).unwrap();
    for i in 0..n {
        inner.store.insert_message(&mut row(conv, format!("{conv}-{i}"), 1_000 + i as i64, read, body(i))).unwrap();
    }
}

fn rss_mb() -> f64 {
    let out = std::process::Command::new("ps").args(["-o", "rss=", "-p", &std::process::id().to_string()]).output().unwrap();
    String::from_utf8_lossy(&out.stdout).trim().parse::<f64>().unwrap_or(0.0) / 1024.0
}

struct Counter(Arc<AtomicUsize>);
impl EventListener for Counter {
    fn on_event(&self, _e: CoreEvent) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

fn count_events(inner: &Inner) -> Arc<AtomicUsize> {
    let n = Arc::new(AtomicUsize::new(0));
    inner.set_listener(Arc::new(Counter(n.clone())));
    n
}

/// 等事件计数稳定（200ms 内不再变化）。
fn settle(n: &AtomicUsize) -> usize {
    let mut last = n.load(Ordering::SeqCst);
    loop {
        std::thread::sleep(Duration::from_millis(200));
        let cur = n.load(Ordering::SeqCst);
        if cur == last {
            return cur;
        }
        last = cur;
    }
}

/// C1：同一 server_ts 连续插入。
#[test]
#[ignore]
fn perf_c1_same_ts_insert() {
    for n in [1000usize, 2000, 5000, 10000] {
        let (inner, _dir) = test_inner();
        inner.store.ensure_conv("u:bob", 0).unwrap();
        let t = Instant::now();
        for i in 0..n {
            inner.store.insert_message(&mut row("u:bob", format!("m{i}"), 5_000, false, text(i))).unwrap();
        }
        let same = t.elapsed();
        let (inner, _dir) = test_inner();
        inner.store.ensure_conv("u:bob", 0).unwrap();
        let t = Instant::now();
        for i in 0..n {
            inner.store.insert_message(&mut row("u:bob", format!("m{i}"), 5_000 + i as i64 * 10, false, text(i))).unwrap();
        }
        println!("C1 同一时间戳 {n} 条: {:.2?}（对照：不同时间戳 {:.2?}）", same, t.elapsed());
    }
}

/// C2：本地搜索。
#[test]
#[ignore]
fn perf_c2_search() {
    for n in [10_000usize, 50_000, 100_000] {
        let (inner, _dir) = test_inner();
        fill(&inner, "u:bob", n, true, text);
        let t = Instant::now();
        let miss = inner.search_messages("不存在的关键词zzz").unwrap().len();
        let d_miss = t.elapsed();
        let t = Instant::now();
        let hit = inner.search_messages("english").unwrap().len();
        println!("C2 搜索 {n} 条: 未命中 {:.2?}（{miss} 条），命中即停 {:.2?}（{hit} 条）", d_miss, t.elapsed());
    }
    // 带 8KB 缩略图的附件
    let n = 50_000usize;
    let (inner, _dir) = test_inner();
    let thumb = crate::util::b64e(vec![7u8; 8 * 1024]);
    fill(&inner, "u:bob", n, true, |i| StoredBody::Attachments {
        items: vec![StoredAttachment {
            wire: WireAttachment {
                id: format!("a{i}"),
                key: "a2V5".into(),
                digest: "ZGlnZXN0".into(),
                size: 5,
                mime: "image/jpeg".into(),
                file_name: format!("IMG_{i}.jpg"),
                kind: AttachmentKind::Image,
                width: 1,
                height: 1,
                duration_ms: 0,
                thumbnail: Some(thumb.clone()),
                format: 2,
            },
            local_path: None,
        }],
        caption: None,
    });
    let before = rss_mb();
    let t = Instant::now();
    let got = inner.search_messages("不存在的关键词zzz").unwrap().len();
    println!("C2 搜索 {n} 条带缩略图附件: {:.2?}（{got} 条），RSS {:+.0}MB", t.elapsed(), rss_mb() - before);
}

/// C3：积压处理（同一发送者 3000 条可见消息）：事件数、回执 POST 数、claim 数。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore]
async fn perf_c3_backlog() {
    use vodozemac::olm::Account;
    for (n, senders) in [(3000usize, 1usize), (100, 1)] {
        let mut alice = Account::new();
        let keys = crate::crypto::olm::prepare_one_time_keys(&mut alice, 1);
        let claim = serde_json::json!({
            "user_id": "alice",
            "identity_key": crate::crypto::olm::identity_key_b64(&alice),
            "signing_key": crate::crypto::olm::signing_key_b64(&alice),
            "device_generation": 1,
            "one_time_key": {"key_id": keys[0].key_id, "key": keys[0].key, "signature": keys[0].signature},
        })
        .to_string();
        let srv = stub::start(Arc::new(move |m, p, _b| match (m, p) {
            ("POST", p) if p.starts_with("/v1/keys/claim/") => (200, claim.clone()),
            ("POST", "/v1/messages") => (200, r#"{"results":[]}"#.into()),
            ("GET", p) if p.starts_with("/v1/users/") => (200, r#"{"id":"alice","name":"Alice"}"#.into()),
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let (inner, _dir) = test_inner_at(&srv.url);
        super::published_account(&inner);
        let events = count_events(&inner);
        let t = Instant::now();
        for i in 0..n {
            let mut c = Content::new(ContentBody::Text { text: format!("积压 {i}"), quote: None, mentions: vec![] }, None, 0);
            c.id = format!("m-{i}");
            let from = format!("alice{}", i % senders).replace("alice0", "alice");
            inner
                .handle_incoming(Incoming { from, content: c, server_ts: 1_000 + i as i64, via_group: None, olm_session_id: Some("s".into()), backlog: true })
                .await
                .unwrap();
        }
        let handled = t.elapsed();
        // 等后台回执发完
        let mut last = usize::MAX;
        loop {
            tokio::time::sleep(Duration::from_millis(700)).await;
            let cur = srv.count("POST", "/v1/messages") + srv.count("POST", "/v1/keys/claim/");
            if cur == last {
                break;
            }
            last = cur;
        }
        let total = t.elapsed() - Duration::from_millis(700);
        let ev = tokio::task::spawn_blocking({
            let e = events.clone();
            move || settle(&e)
        })
        .await
        .unwrap();
        println!(
            "C3 积压 {n} 条: 处理 {:.2?}，含回执收尾 {:.2?}；事件 {ev} 个，回执 POST {} 次，claim {} 次",
            handled,
            total,
            srv.count("POST", "/v1/messages"),
            srv.count("POST", "/v1/keys/claim/")
        );
    }
}

/// C4：mark_read。
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore]
async fn perf_c4_mark_read() {
    let (inner, _dir) = test_inner();
    inner.set_settings(&Settings { read_receipts: false, ..Settings::default() }).unwrap();
    fill(&inner, "u:bob", 5000, false, text);
    let events = count_events(&inner);
    let t = Instant::now();
    inner.mark_read("u:bob").await.unwrap();
    let d = t.elapsed();
    let e = events.clone();
    let ev = tokio::task::spawn_blocking(move || settle(&e)).await.unwrap();
    println!("C4 mark_read 5000 未读: {:.2?}，事件 {ev} 个", d);

    let (inner, _dir) = test_inner();
    fill(&inner, "u:bob", 100_000, true, text);
    let t = Instant::now();
    for _ in 0..10 {
        inner.mark_read("u:bob").await.unwrap();
    }
    println!("C4 mark_read 10 万已读 0 未读: {:.2?}/次", t.elapsed() / 10);
}

/// C5：删除 / 清空 10 万条消息的会话。
#[test]
#[ignore]
fn perf_c5_delete_conversation() {
    let (inner, _dir) = test_inner();
    fill(&inner, "u:bob", 100_000, true, text);
    fill(&inner, "u:carol", 100_000, true, text);
    let t = Instant::now();
    inner.delete_conversation("u:bob").unwrap();
    let d1 = t.elapsed();
    let t = Instant::now();
    inner.clear_conversation("u:carol").unwrap();
    println!("C5 10 万条: delete_conversation {:.2?}，clear_conversation {:.2?}", d1, t.elapsed());
}
