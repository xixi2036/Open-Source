//! 2026-09-30 审查 A 类缺陷的复现测试（`docs/audit/2026-09-30-core.md`）。每个测试在修复前失败。

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use vodozemac::olm::{Account, Session};

use super::secure::Incoming;
use super::{stub, test_group, test_inner, test_inner_at, Inner};
use crate::api::Envelope;
use crate::content::{Content, ContentBody, WireQuote};
use crate::crypto::{megolm, olm};
use crate::store::{IdentityRec, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::{b64e, now_ms};

fn text(t: &str) -> ContentBody {
    ContentBody::Text { text: t.into(), quote: None, mentions: vec![] }
}

/// 给 `inner` 存入一个账户，并返回 alice 到它的出站 Olm 会话（alice 的身份已记入本地，解密 PreKey 时不需要网络）。
fn olm_pair(inner: &Inner) -> Session {
    let mut mine = Account::new();
    mine.generate_one_time_keys(1);
    let otk = *mine.one_time_keys().values().next().unwrap();
    mine.mark_keys_as_published();
    inner.store.save_account(&mine).unwrap();
    let alice = Account::new();
    inner
        .store
        .put_identity(&IdentityRec {
            user_id: "alice".into(),
            identity_key: olm::identity_key_b64(&alice),
            signing_key: olm::signing_key_b64(&alice),
            device_generation: 1,
            verified: false,
        })
        .unwrap();
    olm::create_outbound(&alice, mine.curve25519_key(), otk).unwrap()
}

fn olm_env_raw(id: &str, s: &mut Session, plain: &[u8], server_ts: i64) -> Envelope {
    let (t, bytes) = olm::encrypt(s, plain).unwrap();
    Envelope { id: id.into(), kind: "olm".into(), from: "alice".into(), msg_type: t, group_id: None, session_id: None, body: b64e(bytes), server_ts }
}

fn olm_env(id: &str, s: &mut Session, body: ContentBody) -> Envelope {
    let mut c = Content::new(body, None, 0);
    c.id = format!("c-{id}");
    olm_env_raw(id, s, &c.to_bytes(), 1_000)
}

fn direct(from: &str, id: &str, server_ts: i64, body: ContentBody) -> Incoming {
    let mut c = Content::new(body, None, 0);
    c.id = id.into();
    c.ts = server_ts;
    Incoming { from: from.into(), content: c, server_ts, via_group: None, olm_session_id: Some("s".into()), backlog: false }
}

fn bodies(inner: &Inner, conv: &str) -> Vec<StoredBody> {
    inner.store.messages(conv, None, 100).unwrap().into_iter().map(|m| m.msg.body).collect()
}

/// A1①：一条垃圾信封不能让对端正常会话上的在途消息全变成「无法解密」，旧会话必须保留。
#[tokio::test]
async fn a1_garbage_envelope_keeps_existing_session() {
    let (inner, _dir) = test_inner();
    let mut s = olm_pair(&inner);
    assert!(inner.process_envelope(&olm_env("e0", &mut s, text("第一条")), false).await);
    let garbage = Envelope {
        id: "junk".into(),
        kind: "olm".into(),
        from: "alice".into(),
        msg_type: 1,
        group_id: None,
        session_id: None,
        body: b64e(b"not an olm message"),
        server_ts: 1_001,
    };
    assert!(inner.process_envelope(&garbage, false).await);
    // 等后台的 session_reset 任务跑完（服务器不可达，claim 立即失败）
    tokio::time::sleep(Duration::from_millis(400)).await;
    assert!(!inner.store.load_olm_sessions("alice").unwrap().is_empty(), "解密失败不能删掉旧会话");
    for i in 0..5 {
        assert!(inner.process_envelope(&olm_env(&format!("e{}", i + 1), &mut s, text(&format!("在途 {i}"))), false).await);
    }
    let texts = bodies(&inner, "u:alice").into_iter().filter(|b| matches!(b, StoredBody::Text { .. })).count();
    assert_eq!(texts, 6, "旧会话上的 5 条在途消息都应正常解密");
    assert_eq!(inner.stats.undecryptable.load(Ordering::Relaxed), 1, "只有垃圾信封那一条是无法解密");
}

/// A1②③：解密成功但本版本解析不了的内容不算解密失败——存成「不支持的消息」，不发 session_reset，
/// 会话保留；附件 kind 等会扩展的枚举遇到未知值时兜底，消息照常显示。
#[tokio::test]
async fn a1_unparseable_content_is_unsupported_not_undecryptable() {
    let (inner, _dir) = test_inner();
    let mut s = olm_pair(&inner);
    // 未来版本的附件类型：按文件显示
    let sticker = br#"{"v":1,"id":"m-sticker","ts":1,"body":{"type":"attachment","attachments":[{"id":"a","key":"k","digest":"d","size":3,"kind":"sticker","file_name":"s.webp"}]}}"#;
    assert!(inner.process_envelope(&olm_env_raw("e1", &mut s, sticker, 1_000), false).await);
    // 结构对不上（text 不是字符串）：不支持的消息
    let bad = br#"{"v":9,"id":"m-bad","ts":1,"body":{"type":"text","text":{"rich":[1,2,3]}}}"#;
    assert!(inner.process_envelope(&olm_env_raw("e2", &mut s, bad, 1_001), false).await);
    // 根本不是 JSON
    assert!(inner.process_envelope(&olm_env_raw("e3", &mut s, b"\x00\x01binary", 1_002), false).await);
    tokio::time::sleep(Duration::from_millis(300)).await;

    let b = bodies(&inner, "u:alice");
    assert_eq!(b.len(), 3);
    assert!(matches!(&b[0], StoredBody::Attachments { items, .. } if items[0].wire.kind == AttachmentKind::File), "{:?}", b[0]);
    for x in &b[1..] {
        assert!(matches!(x, StoredBody::System { text } if text == "[不支持的消息，请升级]"), "{x:?}");
    }
    assert_eq!(inner.stats.undecryptable.load(Ordering::Relaxed), 0);
    assert!(inner.reset_sent.lock().unwrap().is_empty(), "解析失败不应请求对方重建会话");
    assert_eq!(inner.store.load_olm_sessions("alice").unwrap().len(), 1);
    // 重投同一信封不重复
    assert!(inner.process_envelope(&olm_env_raw("e2", &mut s, bad, 1_001), false).await);
    assert_eq!(bodies(&inner, "u:alice").len(), 3);
    // 后续消息照常
    assert!(inner.process_envelope(&olm_env("e4", &mut s, text("之后的消息")), false).await);
    assert!(matches!(bodies(&inner, "u:alice").last(), Some(StoredBody::Text { text }) if text == "之后的消息"));
}

/// A2：已存在的坏会话（`u:`）不能让整个会话列表报错。
#[tokio::test]
async fn a2_bad_conversation_row_does_not_break_list() {
    let (inner, _dir) = test_inner();
    inner.insert_system("u:bob", "正常会话").unwrap();
    // 旧版本处理 from 为空的信封后留下的行
    inner.store.debug_exec("INSERT INTO conversations(conv_id,last_ts) VALUES('u:',5)");
    let list = inner.conversations(false).expect("单行出错不应让列表失败");
    assert_eq!(list.iter().map(|c| c.id.as_str()).collect::<Vec<_>>(), vec!["u:bob"]);
    // 不再产生这种会话
    assert!(inner.store.ensure_conv("u:", 0).is_err());
    assert!(inner.store.ensure_conv("g:", 0).is_err());
    assert!(inner.store.ensure_conv("x", 0).is_err());
    let env = Envelope { id: "e".into(), kind: "olm".into(), from: String::new(), msg_type: 1, group_id: None, session_id: None, body: "AA==".into(), server_ts: 1 };
    assert!(inner.process_envelope(&env, false).await, "坏信封直接确认丢弃");
    assert!(inner.store.get_conv("u:").unwrap().is_some_and(|c| c.last_ts == 5), "不应再写入 u: 会话");
    assert!(inner.store.messages("u:", None, 10).unwrap().is_empty());
}

/// A3：mark_read 与并发到达的撤回交错时，已撤回的消息不能「复活」。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a3_mark_read_does_not_resurrect_recalled() {
    let (inner, _dir) = test_inner();
    inner.set_settings(&Settings { read_receipts: false, ..Settings::default() }).unwrap();
    for round in 0..40 {
        let ids: Vec<String> = (0..20).map(|i| format!("r{round}-{i}")).collect();
        for (i, id) in ids.iter().enumerate() {
            inner.handle_incoming(direct("bob", id, now_ms() + i as i64, text("未读"))).await.unwrap();
        }
        let a = inner.clone();
        let reader = tokio::spawn(async move { a.mark_read("u:bob").await.unwrap() });
        let b = inner.clone();
        let ids2 = ids.clone();
        let recaller = tokio::spawn(async move {
            for (i, id) in ids2.iter().enumerate() {
                b.handle_incoming(direct("bob", &format!("d-{id}"), now_ms() + i as i64, ContentBody::Delete { target_id: id.clone() }))
                    .await
                    .unwrap();
            }
        });
        reader.await.unwrap();
        recaller.await.unwrap();
        for id in &ids {
            let m = inner.store.get_message(id).unwrap().unwrap();
            assert!(m.deleted, "第 {round} 轮：{id} 已撤回，不能被已读写回覆盖");
            assert!(matches!(m.msg.body, StoredBody::System { .. }), "撤回的消息正文不能恢复");
        }
    }
}

/// A4：经 Olm 私发、带 group_id 的可见内容不能落进群会话；已退出的群不接受非成员注入。
#[tokio::test]
async fn a4_olm_content_with_group_id_not_routed_to_group() {
    let (inner, _dir) = test_inner();
    inner.store.put_group(&test_group("g1", &["me", "alice", "bob"]), true).unwrap();
    inner.store.put_group(&test_group("g2", &["alice"]), false).unwrap(); // 我已退出
    let with_gid = |from: &str, id: &str, gid: &str, body: ContentBody| {
        let mut i = direct(from, id, 1_000, body);
        i.content.group_id = Some(gid.into());
        i
    };
    inner.handle_incoming(with_gid("alice", "p1", "g1", text("只发给你一个人"))).await.unwrap();
    inner.handle_incoming(with_gid("mallory", "p2", "g2", text("非成员注入"))).await.unwrap();
    assert!(inner.store.messages("g:g1", None, 10).unwrap().is_empty(), "私发内容不能冒充群消息");
    assert!(inner.store.messages("g:g2", None, 10).unwrap().is_empty(), "已退出的群不接受注入");
    assert_eq!(inner.store.messages("u:alice", None, 10).unwrap().len(), 1, "归入与发送者的单聊");
    assert_eq!(inner.store.messages("u:mallory", None, 10).unwrap().len(), 1);

    // 群消息的回执仍经 Olm 到达并生效
    inner.store.put_group(&test_group("g3", &["me", "alice"]), true).unwrap();
    let c = Content::new(text("群里我发的"), Some("g3".into()), 0);
    let row = inner.insert_outgoing("g:g3", &c, StoredMessage::plain(StoredBody::Text { text: "群里我发的".into() }, None)).unwrap();
    inner.store.mark_sent(&row.id, 10).unwrap();
    inner
        .handle_incoming(with_gid("alice", "rc", "g3", ContentBody::Receipt { kind: crate::content::ReceiptKind::Delivered, ids: vec![c.id.clone()] }))
        .await
        .unwrap();
    assert_eq!(inner.store.get_message(&row.id).unwrap().unwrap().status, MessageStatus::Delivered);
}

/// A5：阅后即焚「后写者胜」不能被发送方自报的时间左右：对方时钟快一天时发来的普通消息不能把我的设置改回去；
/// 自报 `i64::MAX` 不能永久锁定设置。
#[tokio::test]
async fn a5_timer_arbitration_ignores_self_reported_future_time() {
    let (inner, _dir) = test_inner();
    let conv = "u:bob";
    let expire = |i: &Inner| i.store.get_conv(conv).unwrap().unwrap().expire_secs;
    let msg = |id: &str, server_ts: i64, expire_secs: u32, set_at: i64, body: ContentBody| {
        let mut i = direct("bob", id, server_ts, body);
        i.content.expire_secs = expire_secs;
        i.content.expire_set_at = set_at;
        i
    };
    let now = now_ms();
    inner.handle_incoming(msg("m1", now - 10_000, 0, 0, text("hi"))).await.unwrap();
    inner.set_timer(conv, 3600, now - 5_000).unwrap();
    // 对方时钟快 1 天：消息里自报的设置时间在「未来」
    inner.handle_incoming(msg("m2", now - 4_000, 0, now + 86_400_000, text("时钟快一天"))).await.unwrap();
    assert_eq!(expire(&inner), 3600, "自报时间在未来的旧设置不能覆盖本地");

    // 对方发 Timer 并自报 i64::MAX：设置生效（它确实更晚），但记下的时间是服务端时间
    inner.handle_incoming(msg("t1", now - 3_000, 60, i64::MAX, ContentBody::Timer { expire_secs: 60 })).await.unwrap();
    assert_eq!(expire(&inner), 60);
    assert_eq!(inner.timer_set_at(conv), now - 3_000, "以信封的服务端时间仲裁");
    // 之后我改设置（更晚的服务端时间）能生效，且对方带着 i64::MAX 的旧消息改不回去
    inner.set_timer(conv, 300, now - 2_000).unwrap();
    inner.handle_incoming(msg("m3", now - 1_000, 60, i64::MAX, text("锁不住"))).await.unwrap();
    assert_eq!(expire(&inner), 300);
    // 晚到的旧 Timer（服务端时间更早）不覆盖
    inner.handle_incoming(msg("t0", now - 9_000, 5, now - 9_000, ContentBody::Timer { expire_secs: 5 })).await.unwrap();
    assert_eq!(expire(&inner), 300);
}

/// A6：极端 server_ts：消息可见、不 panic、会话不会被顶到未来。
#[tokio::test]
async fn a6_extreme_server_ts_clamped() {
    let (inner, _dir) = test_inner();
    inner.handle_incoming(direct("bob", "x1", i64::MAX, text("最大时间戳"))).await.unwrap();
    inner.handle_incoming(direct("bob", "x2", i64::MAX, text("第二条同值"))).await.unwrap();
    // 2100 年
    inner.handle_incoming(direct("carol", "y1", 4_102_444_800_000, text("来自 2100 年"))).await.unwrap();
    let msgs = inner.messages("u:bob", None, 10).unwrap();
    assert_eq!(msgs.len(), 2, "极端时间戳的消息也应可见");
    let limit = now_ms() + 25 * 3600 * 1000;
    assert!(msgs.iter().all(|m| m.sort_ts < limit), "排序时间钳到当前时间附近");
    assert!(msgs[0].sort_ts < msgs[1].sort_ts);
    assert!(inner.conversation("u:carol").unwrap().unwrap().last_activity_ts < limit, "会话不能永远置顶");
    // 信封入口同样钳制
    let raw = serde_json::json!({"id":"e","kind":"olm","from":"6f1b3a52-8a39-4b55-a3f4-0d6a1f5f0c11","type":1,"body":"AA==","server_ts": i64::MAX});
    let env = super::sync::parse_envelope(&raw).unwrap();
    assert!(env.server_ts < limit);
}

/// A7：引用内容要与本地核对：本地有原消息时用本地文本；没有时标「原消息不可用」，不原样保存对方给的文本。
#[tokio::test]
async fn a7_quote_verified_against_local_message() {
    let (inner, _dir) = test_inner();
    inner.handle_incoming(direct("bob", "orig", 1_000, text("bob 的原话"))).await.unwrap();
    let quoted = |id: &str, q: WireQuote| direct("bob", id, 2_000, ContentBody::Text { text: "回复".into(), quote: Some(q), mentions: vec![] });
    let forged_text = "伪造".repeat(25_000);
    inner.handle_incoming(quoted("q1", WireQuote { id: "nope".into(), author: "me".into(), text: forged_text.clone() })).await.unwrap();
    inner.handle_incoming(quoted("q2", WireQuote { id: "orig".into(), author: "bob".into(), text: "篡改成别的话".into() })).await.unwrap();
    // 张冠李戴：把 bob 的消息说成是我发的
    inner.handle_incoming(quoted("q3", WireQuote { id: "orig".into(), author: "me".into(), text: "我从没说过".into() })).await.unwrap();
    let q = |id: &str| inner.store.get_message(id).unwrap().unwrap().msg.quote.unwrap();
    assert_eq!(q("q1").text, "原消息不可用");
    assert_eq!(q("q2").text, "bob 的原话", "本地有原消息时以本地文本为准");
    assert_eq!(q("q3").text, "原消息不可用", "(id, author) 对不上视为不存在");
    assert!(q("q1").text.chars().count() <= 200);
}

fn megolm_env(id: &str, from: &str, gid: &str, sid: &str, bytes: Vec<u8>) -> Envelope {
    Envelope { id: id.into(), kind: "megolm".into(), from: from.into(), msg_type: 0, group_id: Some(gid.into()), session_id: Some(sid.into()), body: b64e(bytes), server_ts: 1_000 }
}

/// A8：已焚毁（或已删除）的群消息在去重记录过期后被重放同一密文，不能重新出现。
#[tokio::test]
async fn a8_megolm_replay_after_seen_expiry_rejected() {
    let (inner, _dir) = test_inner();
    inner.store.put_group(&test_group("g1", &["me", "alice"]), true).unwrap();
    let mut out = megolm::new_outbound();
    let sid = out.session_id();
    let k0 = megolm::export_key(&out);
    inner.accept_room_key("alice", "g1", &sid, &k0).await.unwrap();
    let c = Content::new(text("阅后即焚的群消息"), Some("g1".into()), 0);
    let cipher = megolm::encrypt(&mut out, &c.to_bytes());
    let next = megolm::encrypt(&mut out, &Content::new(text("下一条"), Some("g1".into()), 0).to_bytes());
    assert!(inner.process_envelope(&megolm_env("e1", "alice", "g1", &sid, cipher.clone()), false).await);
    assert_eq!(inner.store.messages("g:g1", None, 10).unwrap().len(), 1);
    // 消息焚毁，30 天后去重记录过期
    inner.store.delete_message(&c.id).unwrap();
    inner.store.prune_seen(i64::MAX).unwrap();
    // 恶意服务端重放同一密文（新信封 id 或原 id 都一样）
    assert!(inner.process_envelope(&megolm_env("e1-replay", "alice", "g1", &sid, cipher.clone()), false).await);
    assert!(inner.process_envelope(&megolm_env("e1", "alice", "g1", &sid, cipher), false).await);
    assert!(inner.store.messages("g:g1", None, 10).unwrap().is_empty(), "重放的密文不能让已焚毁的消息回来");
    // 同一会话后续的新消息不受影响；更早的 room_key 重发也不会清掉已处理位置
    inner.accept_room_key("alice", "g1", &sid, &k0).await.unwrap();
    assert!(inner.process_envelope(&megolm_env("e2", "alice", "g1", &sid, next), false).await);
    assert_eq!(inner.store.messages("g:g1", None, 10).unwrap().len(), 1);
}

/// A9：撤回窗口以服务端时间为准，发送方自报的 ts 不能让 30 天前的消息被撤回。
#[tokio::test]
async fn a9_recall_window_uses_server_time() {
    let (inner, _dir) = test_inner();
    let now = now_ms();
    let old = now - 30 * 24 * 3600 * 1000;
    inner.handle_incoming(direct("bob", "old", old, text("30 天前"))).await.unwrap();
    inner.handle_incoming(direct("bob", "new", now - 60_000, text("刚才"))).await.unwrap();
    // 撤回内容里的 ts 伪造成原消息之后 1 秒；信封的服务端时间是现在
    let mut d = direct("bob", "d1", now, ContentBody::Delete { target_id: "old".into() });
    d.content.ts = old + 1_000;
    inner.handle_incoming(d).await.unwrap();
    assert!(!inner.store.get_message("old").unwrap().unwrap().deleted, "超过 24 小时的消息不能被撤回");
    inner.handle_incoming(direct("bob", "d2", now, ContentBody::Delete { target_id: "new".into() })).await.unwrap();
    assert!(inner.store.get_message("new").unwrap().unwrap().deleted, "窗口内照常撤回");
}

/// A10：本地删除未读消息后未读数回落；撤回、阅后即焚同理。
#[tokio::test]
async fn a10_unread_count_follows_deletions() {
    let (inner, _dir) = test_inner();
    let now = now_ms();
    inner.handle_incoming(direct("bob", "u1", now - 3_000, text("未读 1"))).await.unwrap();
    inner.handle_incoming(direct("bob", "u2", now - 2_000, text("未读 2"))).await.unwrap();
    inner.handle_incoming(direct("bob", "u3", now - 1_000, text("未读 3"))).await.unwrap();
    let unread = |i: &Inner| i.conversation("u:bob").unwrap().unwrap().unread_count;
    assert_eq!(unread(&inner), 3);
    inner.delete_local("u1").unwrap();
    assert_eq!(unread(&inner), 2, "删掉未读消息后未读数应回落");
    inner.handle_incoming(direct("bob", "d", now, ContentBody::Delete { target_id: "u2".into() })).await.unwrap();
    assert_eq!(unread(&inner), 1, "对方撤回的未读消息不再计数");
    inner.clear_conversation("u:bob").unwrap();
    assert_eq!(unread(&inner), 0);
}

/// A11：群成员 claim 恒失败（404 / 403）时，不能每条群消息都再 claim 两次。
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a11_failing_member_claim_is_negatively_cached() {
    let srv = stub::start(Arc::new(|m, p, _b| match (m, p) {
        ("POST", p) if p.starts_with("/v1/keys/claim/") => (404, r#"{"error":"not_found","message":"该用户没有可用的预密钥"}"#.into()),
        ("POST", "/v1/groups/g1/messages") => (200, r#"{"id":"x","server_ts":5}"#.into()),
        _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
    }))
    .await;
    let (inner, _dir) = test_inner_at(&srv.url);
    super::published_account(&inner);
    inner.store.put_group(&test_group("g1", &["me", "carol"]), true).unwrap();
    for i in 0..5 {
        inner.send_text("g:g1", format!("群消息 {i}"), None).await.unwrap();
    }
    assert_eq!(srv.count("POST", "/v1/groups/g1/messages"), 5);
    let claims = srv.count("POST", "/v1/keys/claim/carol");
    assert!(claims <= 1, "5 条群消息对失败成员 claim 了 {claims} 次");
}
