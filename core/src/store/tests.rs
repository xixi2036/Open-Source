//! 本地存储的单元测试。

use super::*;

fn store() -> Store {
    Store::open_in_memory(DbCipher::new(&[9u8; 32]).unwrap()).unwrap()
}

fn row(id: &str, text: &str, ts: i64) -> MsgRow {
    MsgRow {
        id: id.into(),
        conv_id: "u:bob".into(),
        sender_id: "bob".into(),
        outgoing: false,
        sent_at: ts,
        server_ts: ts,
        sort_ts: ts,
        status: MessageStatus::Delivered,
        msg: StoredMessage::plain(StoredBody::Text { text: text.into() }, None),
        expire_secs: 0,
        expires_at: None,
        deleted: false,
        read: false,
        mentions_me: false,
    }
}

#[test]
fn message_body_encrypted_at_rest() {
    let s = store();
    s.ensure_conv("u:bob", 0).unwrap();
    assert!(s.insert_message(&mut row("m1", "绝密内容 top-secret", 10)).unwrap());
    assert!(!s.insert_message(&mut row("m1", "dup", 10)).unwrap(), "按 id 去重");
    let raw = s.raw_message_body("m1").unwrap().unwrap();
    let hay = String::from_utf8_lossy(&raw);
    assert!(!hay.contains("top-secret"));
    let m = s.get_message("m1").unwrap().unwrap();
    assert_eq!(m.msg.body, StoredBody::Text { text: "绝密内容 top-secret".into() });
    // 用错误密钥无法解密
    let other = DbCipher::new(&[1u8; 32]).unwrap();
    assert!(other.open(LABEL_BODY, &raw).is_err());
}

#[test]
fn migrations_by_user_version() {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("db.sqlite");
    let key = || DbCipher::new(&[9u8; 32]).unwrap();

    // 模拟引入迁移框架之前的旧库：表已存在、user_version=0、有数据
    {
        let c = Connection::open(&p).unwrap();
        c.execute_batch(SCHEMA).unwrap();
        c.execute("INSERT INTO friends(user_id) VALUES('old-friend')", []).unwrap();
        assert_eq!(user_version(&c).unwrap(), 0);
    }
    {
        let s = Store::open(&p, key()).unwrap();
        assert_eq!(user_version(&s.conn()).unwrap(), SCHEMA_VERSION);
        assert_eq!(SCHEMA_VERSION, 4);
        assert_eq!(s.friend_ids().unwrap(), vec!["old-friend"], "旧数据应保留");
    }

    // 追加一个新迁移：只执行一次
    const NOTE: &str = "ALTER TABLE friends ADD COLUMN note TEXT NOT NULL DEFAULT '';";
    let mut extra: Vec<Migration> = MIGRATIONS.to_vec();
    extra.push(Migration::Sql(NOTE));
    {
        let mut c = Connection::open(&p).unwrap();
        migrate_with(&mut c, &extra, &key()).unwrap();
        assert_eq!(user_version(&c).unwrap(), SCHEMA_VERSION + 1);
        migrate_with(&mut c, &extra, &key()).unwrap(); // 再次执行是空操作（否则 ADD COLUMN 会报重复列）
        let note: String = c.query_row("SELECT note FROM friends WHERE user_id='old-friend'", [], |r| r.get(0)).unwrap();
        assert_eq!(note, "");
    }

    // 失败的迁移回滚，版本不变
    {
        let mut c = Connection::open(&p).unwrap();
        let mut bad = extra.clone();
        bad.push(Migration::Sql("CREATE TABLE t(x); SELECT * FROM no_such_table;"));
        assert!(migrate_with(&mut c, &bad, &key()).is_err());
        assert_eq!(user_version(&c).unwrap(), SCHEMA_VERSION + 1);
        assert!(c.query_row("SELECT 1 FROM sqlite_master WHERE name='t'", [], |_| Ok(())).optional().unwrap().is_none(), "失败迁移应整体回滚");
    }

    // 库版本比代码新：拒绝打开（防止旧版本核心写乱新结构）
    assert!(Store::open(&p, key()).is_err());
}

/// 旧版本（v1）的库：通话记录没有结束原因、附件没有 format、没有 inbox 表。
fn old_v1_db(p: &Path, cipher: &DbCipher) {
    let c = Connection::open(p).unwrap();
    c.execute_batch(SCHEMA).unwrap();
    c.pragma_update(None, "user_version", 1).unwrap();
    c.execute("INSERT INTO kv(k,v) VALUES('key_check',?1)", params![cipher.seal("key_check", b"hush")]).unwrap();
    let insert = |id: &str, ts: i64, body: serde_json::Value| {
        let blob = cipher.seal(LABEL_BODY, &serde_json::to_vec(&body).unwrap());
        c.execute(
            "INSERT INTO messages(id,conv_id,sender_id,outgoing,sent_at,sort_ts,status,body) VALUES(?1,'u:bob','bob',0,?2,?2,'read',?3)",
            params![id, ts, blob],
        )
        .unwrap();
    };
    // 旧代码写出的 JSON（与当时的 StoredMessage 序列化一致）
    let call = |d: u32, missed: bool, outgoing: bool| {
        serde_json::json!({"body":{"t":"call_log","video":false,"duration_secs":d,"missed":missed,"outgoing":outgoing},"quote":null})
    };
    insert("c-completed", 1, call(42, false, true));
    insert("c-missed", 2, call(0, true, false));
    insert("c-cancelled", 3, call(0, false, true));
    insert(
        "att-old",
        4,
        serde_json::json!({"body":{"t":"attachments","items":[{"id":"a","key":"k","digest":"d","size":3,"mime":"","file_name":"x","kind":"file","width":0,"height":0,"duration_ms":0,"local_path":null}],"caption":null},"quote":null}),
    );
    insert("text", 5, serde_json::json!({"body":{"t":"text","text":"旧消息"},"quote":null}));
    // 一条正文损坏的通话记录：迁移跳过，不影响其他行
    c.execute(
        "INSERT INTO messages(id,conv_id,sender_id,outgoing,sent_at,sort_ts,status,body) VALUES('broken','u:bob','bob',0,6,6,'read',x'00010203')",
        [],
    )
    .unwrap();
}

/// v1 → v3：inbox 表建立；旧通话记录按规则补上结束原因（并真正写回库里）；旧附件 format 缺省为 1；其余数据完好。
#[test]
fn migrate_v1_to_current_keeps_data() {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("db.sqlite");
    let cipher = DbCipher::new(&[9u8; 32]).unwrap();
    old_v1_db(&p, &cipher);
    let s = Store::open(&p, cipher.clone()).unwrap();
    assert_eq!(user_version(&s.conn()).unwrap(), SCHEMA_VERSION);
    assert_eq!(s.inbox_count().unwrap(), 0, "inbox 表已建立");

    // 迁移已把 reason 写进正文（不是读取时推断）
    let stored_reason = |id: &str| match s.get_message(id).unwrap().unwrap().msg.body {
        StoredBody::CallLog { reason, .. } => reason,
        other => panic!("{id} 应为通话记录: {other:?}"),
    };
    assert_eq!(stored_reason("c-completed"), Some(CallEndReason::Completed));
    assert_eq!(stored_reason("c-missed"), Some(CallEndReason::Missed));
    assert_eq!(stored_reason("c-cancelled"), Some(CallEndReason::Cancelled));
    match s.get_message("c-missed").unwrap().unwrap().msg.body.to_api() {
        MessageBody::CallLog { reason, missed, .. } => assert_eq!((reason, missed), (CallEndReason::Missed, true)),
        _ => unreachable!(),
    }
    match &s.get_message("att-old").unwrap().unwrap().msg.body {
        StoredBody::Attachments { items, .. } => assert_eq!(items[0].wire.format, 1, "旧附件缺省为格式 v1"),
        other => panic!("{other:?}"),
    }
    assert!(matches!(s.get_message("text").unwrap().unwrap().msg.body, StoredBody::Text { ref text } if text == "旧消息"));
    assert!(matches!(s.get_message("broken").unwrap().unwrap().msg.body, StoredBody::Undecryptable));
    assert_eq!(s.messages("u:bob", None, 50).unwrap().len(), 6);
    drop(s);
    // 再次打开：已是最新版本，不重复迁移
    let s = Store::open(&p, cipher).unwrap();
    assert_eq!(user_version(&s.conn()).unwrap(), SCHEMA_VERSION);
}

/// 没有 reason 的通话记录（例如迁移时正文无法解密、之后又恢复）读取时按同一规则兜底。
#[test]
fn call_log_reason_fallback_on_read() {
    for (d, missed, want) in [(5, false, CallEndReason::Completed), (0, true, CallEndReason::Missed), (0, false, CallEndReason::Cancelled)] {
        let b = StoredBody::CallLog { video: true, duration_secs: d, missed, outgoing: false, reason: None };
        assert!(matches!(b.to_api(), MessageBody::CallLog { reason, .. } if reason == want));
    }
}

/// 用错误密钥打开需要数据迁移的旧库：返回 KeyMismatch，且不执行迁移（版本不变、数据不被改写）。
#[test]
fn wrong_key_on_old_db_is_key_mismatch_and_not_migrated() {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("db.sqlite");
    let cipher = DbCipher::new(&[9u8; 32]).unwrap();
    old_v1_db(&p, &cipher);
    let err = Store::open(&p, DbCipher::new(&[8u8; 32]).unwrap()).err().expect("错误密钥必须拒绝");
    assert!(matches!(&err, HushError::KeyMismatch { message } if message == "db_key 与已有数据库不匹配"), "{err:?}");
    let c = Connection::open(&p).unwrap();
    assert_eq!(user_version(&c).unwrap(), 1, "密钥不符时不能执行任何迁移");
    drop(c);
    assert!(Store::open(&p, cipher).is_ok());
}

/// 收件箱：信封先落库；明文与会话在同一事务写入；完成后记 seen 并删除。
#[test]
fn inbox_roundtrip() {
    let s = store();
    let env = crate::api::Envelope {
        id: "e1".into(),
        kind: "olm".into(),
        from: "alice".into(),
        msg_type: 1,
        group_id: None,
        session_id: None,
        body: "QUJD".into(),
        server_ts: 7,
    };
    s.inbox_put(&env).unwrap();
    s.inbox_put(&env).unwrap();
    assert_eq!(s.inbox_count().unwrap(), 1);
    assert!(s.inbox_plaintext("e1").unwrap().is_none());
    let plain = InboxPlain { plaintext: "c2VjcmV0LXBsYWlu".into(), olm_session_id: Some("sid".into()) };
    {
        let c = s.conn();
        s.write_inbox_plain(&c, "e1", &plain).unwrap();
    }
    assert_eq!(s.inbox_plaintext("e1").unwrap(), Some(plain));
    let raw: Vec<u8> = s.conn().query_row("SELECT plaintext FROM inbox WHERE envelope_id='e1'", [], |r| r.get(0)).unwrap();
    assert!(!String::from_utf8_lossy(&raw).contains("c2VjcmV0"), "明文字段加密存储");
    let listed = s.inbox_list().unwrap();
    assert_eq!((listed.len(), listed[0].server_ts, listed[0].msg_type), (1, 7, 1));
    // wipe_crypto 保留已解密的行
    s.wipe_crypto().unwrap();
    assert_eq!(s.inbox_count().unwrap(), 1);
    s.finish_envelope("env:e1", "e1").unwrap();
    assert_eq!(s.inbox_count().unwrap(), 0);
    assert!(s.is_seen("env:e1").unwrap());
}

/// 资料缓存（C-C / K2）：非权威来源既不抹掉、也不改写已缓存的邮箱；权威来源与删好友才能改。
#[test]
fn profile_email_cache_rules() {
    let s = store();
    let p = |email: Option<&str>| Profile { id: "u1".into(), name: "n".into(), email: email.map(String::from), ..Default::default() };
    s.put_profile_authoritative(&p(Some("a@x"))).unwrap();
    s.put_profile(&p(None)).unwrap();
    assert_eq!(s.get_profile("u1").unwrap().unwrap().email.as_deref(), Some("a@x"), "群成员 / 朋友圈来源不带邮箱");
    s.set_friends(&[p(Some("a@x"))]).unwrap();
    s.set_friends(&[]).unwrap();
    assert_eq!(s.get_profile("u1").unwrap().unwrap().email, None, "不再是好友：清掉邮箱");
    s.put_profile_authoritative(&p(Some("a@x"))).unwrap();
    s.put_profile_authoritative(&p(None)).unwrap();
    assert_eq!(s.get_profile("u1").unwrap().unwrap().email, None);
    s.put_profile(&p(Some("b@x"))).unwrap();
    assert_eq!(s.get_profile("u1").unwrap().unwrap().email, None, "非权威来源不能写入邮箱");
    s.put_profile_authoritative(&p(Some("a@x"))).unwrap();
    let merged = s.put_profile(&p(Some("evil@x"))).unwrap();
    assert_eq!(merged.email.as_deref(), Some("a@x"), "也不能改写，返回沿用后的资料");
    assert_eq!(s.get_profile("u1").unwrap().unwrap().email.as_deref(), Some("a@x"));
    s.add_friend("u1").unwrap();
    s.remove_friend("u1").unwrap();
    assert_eq!(s.get_profile("u1").unwrap().unwrap().email, None);
}

#[test]
fn same_id_from_different_senders_both_kept() {
    let s = store();
    s.ensure_conv("g:x", 0).unwrap();
    let mut a = row("m1", "来自 alice", 1);
    a.conv_id = "g:x".into();
    a.sender_id = "alice".into();
    let mut b = row("m1", "来自 bob（撞 id）", 2);
    b.conv_id = "g:x".into();
    b.sender_id = "bob".into();
    assert!(s.insert_incoming(&mut a).unwrap());
    assert!(s.insert_incoming(&mut b).unwrap(), "不同发送者的相同 id 不能被丢弃");
    assert_eq!(a.id, "m1");
    assert_eq!(b.id, "bob:m1");
    assert_eq!(b.content_id(), "m1");
    // 同一发送者重复：去重
    let mut b2 = b.clone();
    b2.id = "m1".into();
    assert!(!s.insert_incoming(&mut b2).unwrap());
    let mut a2 = a.clone();
    assert!(!s.insert_incoming(&mut a2).unwrap());
    // 按 (内容 id, 发送者) 查找互不串
    assert_eq!(s.find_by_content(Some("g:x"), "m1", "alice").unwrap().unwrap().id, "m1");
    assert_eq!(s.find_by_content(Some("g:x"), "m1", "bob").unwrap().unwrap().id, "bob:m1");
    assert!(s.find_by_content(Some("g:x"), "m1", "carol").unwrap().is_none());
    assert!(s.find_by_content(Some("u:other"), "m1", "alice").unwrap().is_none());
    assert_eq!(s.messages("g:x", None, 10).unwrap().len(), 2);
}

/// 单行正文损坏不影响同一查询里的其他行。
#[test]
fn corrupt_row_shown_as_undecryptable() {
    let s = store();
    s.insert_message(&mut row("a", "好的", 1)).unwrap();
    s.insert_message(&mut row("b", "坏的", 2)).unwrap();
    s.conn().execute("UPDATE messages SET body=x'00010203' WHERE id='b'", []).unwrap();
    let msgs = s.messages("u:bob", None, 10).unwrap();
    assert_eq!(msgs.len(), 2);
    assert!(matches!(msgs[0].msg.body, StoredBody::Text { ref text } if text == "好的"));
    assert!(matches!(msgs[1].msg.body, StoredBody::Undecryptable));
    assert!(s.last_message("u:bob").unwrap().is_some());
}

#[test]
fn content_id_validation() {
    assert!(valid_content_id("a4d0a1a2-7d0e-4c1b-9d1a-0c2b3e4f5a6b"));
    assert!(valid_content_id("abc_DEF-123"));
    assert!(!valid_content_id(""));
    assert!(!valid_content_id("alice:m1"), "不能含冒号，否则可伪造派生 id");
    assert!(!valid_content_id(&"a".repeat(129)));
    assert!(!valid_content_id("含中文"));
}

#[test]
fn attachment_key_encrypted_at_rest() {
    let s = store();
    let mut m = row("m2", "", 1);
    m.msg.body = StoredBody::Attachments {
        items: vec![StoredAttachment {
            wire: WireAttachment {
                id: "att".into(),
                key: "S0VZLVNFQ1JFVC1LRVktU0VDUkVULUtFWS1TRUNSRVQ=".into(),
                digest: "DIGEST".into(),
                size: 1,
                mime: "image/png".into(),
                file_name: "a.png".into(),
                kind: AttachmentKind::Image,
                width: 0,
                height: 0,
                duration_ms: 0,
                thumbnail: None,
                format: 2,
            },
            local_path: None,
        }],
        caption: None,
    };
    s.insert_message(&mut m).unwrap();
    let raw = s.raw_message_body("m2").unwrap().unwrap();
    assert!(!String::from_utf8_lossy(&raw).contains("S0VZLVNFQ1JFVC1LRVkt"));
    assert_eq!(s.get_message("m2").unwrap().unwrap().msg, m.msg);
}

#[test]
fn pagination_and_expiry() {
    let s = store();
    s.ensure_conv("u:bob", 0).unwrap();
    for i in 0..10 {
        let mut r = row(&format!("m{i}"), "x", i as i64 * 10);
        if i == 3 {
            r.expires_at = Some(5);
        }
        s.insert_message(&mut r).unwrap();
    }
    let last3 = s.messages("u:bob", None, 3).unwrap();
    assert_eq!(last3.iter().map(|m| m.id.as_str()).collect::<Vec<_>>(), vec!["m7", "m8", "m9"]);
    let older = s.messages("u:bob", Some(70), 2).unwrap();
    assert_eq!(older.iter().map(|m| m.id.as_str()).collect::<Vec<_>>(), vec!["m5", "m6"]);
    assert_eq!(s.expired_messages(6).unwrap(), vec![("m3".to_string(), "u:bob".to_string())]);
    assert!(s.expired_messages(4).unwrap().is_empty());
}

#[test]
fn reactions_grouped() {
    let s = store();
    s.set_reaction("m", "a", "👍").unwrap();
    s.set_reaction("m", "b", "👍").unwrap();
    s.set_reaction("m", "c", "❤️").unwrap();
    s.set_reaction("m", "b", "❤️").unwrap(); // 每人一个回应，替换
    let r = s.reactions("m").unwrap();
    assert_eq!(r.len(), 2);
    let thumbs = r.iter().find(|g| g.emoji == "👍").unwrap();
    assert_eq!(thumbs.user_ids, vec!["a"]);
    s.remove_reaction("m", "c", "❤️").unwrap();
    assert_eq!(s.reactions("m").unwrap().iter().find(|g| g.emoji == "❤️").unwrap().user_ids, vec!["b"]);
}

#[test]
fn kv_json_encrypted_and_key_check() {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("db.sqlite");
    {
        let s = Store::open(&p, DbCipher::new(&[9u8; 32]).unwrap()).unwrap();
        s.kv_set_json("session", &serde_json::json!({"token":"tok-secret"})).unwrap();
        let raw = s.kv_get("session").unwrap().unwrap();
        assert!(!String::from_utf8_lossy(&raw).contains("tok-secret"));
        let v: serde_json::Value = s.kv_get_json("session").unwrap().unwrap();
        assert_eq!(v["token"], "tok-secret");
    }
    assert!(matches!(Store::open(&p, DbCipher::new(&[8u8; 32]).unwrap()), Err(HushError::KeyMismatch { .. })));
    assert!(Store::open(&p, DbCipher::new(&[9u8; 32]).unwrap()).is_ok());
}

#[test]
fn olm_and_megolm_pickles_persist() {
    let s = store();
    let acc = vodozemac::olm::Account::new();
    s.save_account(&acc).unwrap();
    let back = s.load_account().unwrap().unwrap();
    assert_eq!(back.curve25519_key(), acc.curve25519_key());
    let raw = s.kv_get("account").unwrap().unwrap();
    // pickle 已加密：不含明文 JSON 字段名
    assert!(!String::from_utf8_lossy(&raw).contains("signing_key"));

    let mut out = OutboundRec { session: megolm::new_outbound(), created_at: 1, msg_count: 0, shared_with: HashSet::new() };
    out.shared_with.insert("bob".into());
    s.save_outbound("g1", &out).unwrap();
    let o2 = s.get_outbound("g1").unwrap().unwrap();
    assert_eq!(o2.session.session_id(), out.session.session_id());
    s.unshare_user("bob").unwrap();
    assert!(s.get_outbound("g1").unwrap().unwrap().shared_with.is_empty());
}

#[test]
fn olm_sessions_pruned_to_limit() {
    let s = store();
    let alice = vodozemac::olm::Account::new();
    for i in 0..8 {
        let mut bob = vodozemac::olm::Account::new();
        let keys = olm::prepare_one_time_keys(&mut bob, 1);
        let otk = olm::parse_curve25519(&keys[0].key).unwrap();
        let session = olm::create_outbound(&alice, bob.curve25519_key(), otk).unwrap();
        s.save_olm_session("peer", &olm::SessEntry { session, last_used: i }).unwrap();
    }
    let left = s.load_olm_sessions("peer").unwrap();
    assert_eq!(left.len() as i64, MAX_OLM_SESSIONS);
    assert!(left.iter().all(|e| e.last_used >= 3));
}

/// 构造一个 v3 的旧库：在 v1 旧库上执行 v2、v3 迁移，然后按 v3 的表结构写入数据
/// （已发送消息 sort_ts 为本地时间、同一会话内有重复的 sort_ts、失败消息没有 server_ts）。
fn old_v3_db(p: &Path, cipher: &DbCipher) {
    old_v1_db(p, cipher);
    let mut c = Connection::open(p).unwrap();
    migrate_with(&mut c, &MIGRATIONS[..3], cipher).unwrap();
    assert_eq!(user_version(&c).unwrap(), 3);
    c.execute("INSERT INTO friends(user_id) VALUES('bob')", []).unwrap();
    c.execute("INSERT INTO conversations(conv_id,last_ts,unread,pinned,muted) VALUES('u:bob',100,2,1,1)", []).unwrap();
    c.execute("INSERT INTO conversations(conv_id,last_ts) VALUES('g:g1',50)", []).unwrap();
    let insert = |id: &str, conv: &str, outgoing: i64, server_ts: i64, sort_ts: i64, status: &str, text: &str| {
        let body = serde_json::json!({"body":{"t":"text","text":text},"quote":null});
        let blob = cipher.seal(LABEL_BODY, &serde_json::to_vec(&body).unwrap());
        c.execute(
            "INSERT INTO messages(id,conv_id,sender_id,outgoing,sent_at,server_ts,sort_ts,status,body) VALUES(?1,?2,'x',?3,?5,?4,?5,?6,?7)",
            params![id, conv, outgoing, server_ts, sort_ts, status, blob],
        )
        .unwrap();
    };
    // 发出的消息：本地时钟快了 30ms，服务端时间 1000
    insert("out-sent", "g:g1", 1, 1000, 1030, "delivered", "我发的");
    insert("in-1", "g:g1", 0, 1010, 1010, "read", "对方 1010");
    insert("in-2", "g:g1", 0, 1020, 1020, "read", "对方 1020");
    // 失败的消息没有 server_ts：保持本地时间
    insert("out-failed", "g:g1", 1, 0, 1025, "failed", "没发出去");
    // 同一毫秒的三条（旧数据可能存在）
    insert("tie-a", "g:g1", 0, 2000, 2000, "read", "a");
    insert("tie-b", "g:g1", 0, 2000, 2000, "read", "b");
    insert("tie-c", "g:g1", 0, 2001, 2001, "read", "c");
}

/// v3 → v4（K3/K5/K6）：已发送消息的 sort_ts 回填为 server_ts；失败的不动；同一会话内重复的 sort_ts 被错开；
/// 新列 / 新表可用；原有数据（消息、好友、会话标记）完好。
#[test]
fn migrate_v3_to_v4_backfills_sort_ts() {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("db.sqlite");
    let cipher = DbCipher::new(&[9u8; 32]).unwrap();
    old_v3_db(&p, &cipher);
    let s = Store::open(&p, cipher.clone()).unwrap();
    assert_eq!(user_version(&s.conn()).unwrap(), 4);
    let sort = |id: &str| s.get_message(id).unwrap().unwrap().sort_ts;
    assert_eq!(sort("out-sent"), 1000, "已发送：回填为服务端时间");
    assert_eq!(sort("out-failed"), 1025, "失败的消息没有 server_ts，保持本地时间");
    assert_eq!(sort("in-1"), 1010);
    let ids: Vec<String> = s.messages("g:g1", None, 50).unwrap().into_iter().map(|m| m.id).collect();
    assert_eq!(ids, vec!["out-sent", "in-1", "in-2", "out-failed", "tie-a", "tie-b", "tie-c"], "按服务端时间排序");
    let ties: Vec<i64> = ["tie-a", "tie-b", "tie-c"].iter().map(|i| sort(i)).collect();
    assert_eq!(ties, vec![2000, 2001, 2002], "同一会话内的重复 sort_ts 依次错开");
    // 逐页（每页 1 条）取完：不漏不重
    let mut seen = Vec::new();
    let mut cursor = None;
    loop {
        let page = s.messages("g:g1", cursor, 1).unwrap();
        let Some(m) = page.first() else { break };
        cursor = Some(m.sort_ts);
        seen.push(m.id.clone());
    }
    seen.reverse();
    assert_eq!(seen, ids);
    // 其余数据完好
    assert!(matches!(s.get_message("text").unwrap().unwrap().msg.body, StoredBody::Text { ref text } if text == "旧消息"));
    assert_eq!(s.friend_ids().unwrap(), vec!["bob"]);
    let c = s.get_conv("u:bob").unwrap().unwrap();
    assert_eq!((c.last_ts, c.unread, c.pinned, c.muted, c.unread_mentions), (100, 2, true, true, 0));
    assert!(c.draft.is_none());
    assert!(!s.get_message("in-1").unwrap().unwrap().mentions_me);
    s.set_draft("u:bob", Some("草稿"), 5).unwrap();
    assert_eq!(s.get_draft("u:bob").unwrap().as_deref(), Some("草稿"));
    drop(s);
    let s = Store::open(&p, cipher).unwrap();
    assert_eq!(user_version(&s.conn()).unwrap(), 4, "再次打开不重复迁移");
    assert_eq!(s.get_draft("u:bob").unwrap().as_deref(), Some("草稿"));
}

/// K3：同一会话内 sort_ts 唯一（同一毫秒依次错开）；发出的消息被服务端接受后 sort_ts 改为 server_ts，
/// 以 sort_ts 为游标逐页翻完仍不漏不重、顺序与服务端时间一致。
#[test]
fn sort_ts_unique_and_pagination_after_mark_sent() {
    let s = store();
    s.ensure_conv("u:bob", 0).unwrap();
    let mut all = Vec::new();
    // 收到的消息（server_ts）与发出的消息（本地时间，时钟快 7ms）交错，还有同一毫秒的
    for i in 0..20i64 {
        let mut r = row(&format!("in{i}"), "x", 1000 + i * 10);
        assert!(s.insert_message(&mut r).unwrap());
        all.push(r.id.clone());
        let mut o = row(&format!("out{i}"), "y", 1000 + i * 10 + 12);
        o.outgoing = true;
        o.sender_id = "me".into();
        o.server_ts = 0;
        o.status = MessageStatus::Sending;
        assert!(s.insert_message(&mut o).unwrap());
        all.push(o.id.clone());
    }
    let mut dup = row("dup", "z", 1000);
    assert!(s.insert_message(&mut dup).unwrap());
    assert_eq!(dup.sort_ts, 1001, "与 in0 同一毫秒：错开 1ms 并写回");
    all.push("dup".into());
    // 发出的消息依次被服务端接受：server_ts = 本地时间 - 7（其中 out3 恰好与 in3 同一毫秒）
    for i in 0..20i64 {
        let st = if i == 3 { 1030 } else { 1000 + i * 10 + 5 };
        let new_sort = s.mark_sent(&format!("out{i}"), st).unwrap().unwrap();
        let m = s.get_message(&format!("out{i}")).unwrap().unwrap();
        assert_eq!((m.server_ts, m.sort_ts, m.status), (st, new_sort, MessageStatus::Sent));
        if i == 3 {
            assert_eq!(new_sort, 1031, "与已有消息同一毫秒：错开");
        } else {
            assert_eq!(new_sort, st);
        }
    }
    // 已送达的消息被（重复的）接受结果更新时不会降级状态
    s.conn().execute("UPDATE messages SET status='read' WHERE id='out0'", []).unwrap();
    s.mark_sent("out0", 1005).unwrap();
    assert_eq!(s.get_message("out0").unwrap().unwrap().status, MessageStatus::Read);
    assert!(s.mark_sent("nope", 1).unwrap().is_none());

    let full = s.messages("u:bob", None, 500).unwrap();
    assert_eq!(full.len(), all.len());
    assert!(full.windows(2).all(|w| w[0].sort_ts < w[1].sort_ts), "会话内 sort_ts 严格递增（唯一）");
    for limit in [1u32, 3, 7] {
        let mut got: Vec<String> = Vec::new();
        let mut cursor = None;
        loop {
            let page = s.messages("u:bob", cursor, limit).unwrap();
            if page.is_empty() {
                break;
            }
            cursor = Some(page[0].sort_ts);
            got.splice(0..0, page.into_iter().map(|m| m.id));
        }
        assert_eq!(got, full.iter().map(|m| m.id.clone()).collect::<Vec<_>>(), "每页 {limit} 条：不漏不重");
    }
}

/// K5：草稿字段加密；内容不变时不刷新时间；列表排序取「最后消息」与「草稿」较晚者，置顶优先。
#[test]
fn drafts_encrypted_and_affect_order() {
    let s = store();
    for (c, ts) in [("u:a", 100), ("u:b", 200), ("u:c", 300)] {
        s.ensure_conv(c, 0).unwrap();
        s.bump_conv(c, ts, 0).unwrap();
        s.conn().execute("UPDATE conversations SET last_ts=?2 WHERE conv_id=?1", params![c, ts]).unwrap();
    }
    let order = || s.list_convs().unwrap().into_iter().map(|c| c.conv_id).collect::<Vec<_>>();
    assert_eq!(order(), vec!["u:c", "u:b", "u:a"]);
    assert!(s.set_draft("u:a", Some("秘密草稿 top-secret"), 400).unwrap());
    let raw: Vec<u8> = s.conn().query_row("SELECT text FROM drafts WHERE conv_id='u:a'", [], |r| r.get(0)).unwrap();
    assert!(!String::from_utf8_lossy(&raw).contains("top-secret"), "草稿加密存储");
    assert_eq!(order(), vec!["u:a", "u:c", "u:b"], "有草稿的会话按草稿时间参与排序");
    assert!(!s.set_draft("u:a", Some("秘密草稿 top-secret"), 999).unwrap(), "内容没变：不刷新时间");
    assert_eq!(s.get_conv("u:a").unwrap().unwrap().draft.unwrap().1, 400);
    s.set_conv_flag("u:b", "pinned", true).unwrap();
    assert_eq!(order(), vec!["u:b", "u:a", "u:c"], "置顶优先规则不变");
    assert!(s.set_draft("u:a", Some("   "), 500).unwrap(), "空白即清除");
    assert!(s.get_draft("u:a").unwrap().is_none());
    assert!(!s.set_draft("u:a", None, 600).unwrap());
    assert_eq!(order(), vec!["u:b", "u:c", "u:a"]);
    // 损坏的草稿视为没有
    s.set_draft("u:c", Some("x"), 1).unwrap();
    s.conn().execute("UPDATE drafts SET text=x'0001' WHERE conv_id='u:c'", []).unwrap();
    assert!(s.get_draft("u:c").unwrap().is_none());
}

/// K1（存储层）：删除会话删掉消息、回应、草稿与会话行；免打扰设置在会话重新出现时沿用。
#[test]
fn delete_conversation_store() {
    let s = store();
    s.ensure_conv("g:x", 0).unwrap();
    s.set_conv_flag("g:x", "muted", true).unwrap();
    s.set_conv_flag("g:x", "pinned", true).unwrap();
    let mut m = row("m1", "hi", 1);
    m.conv_id = "g:x".into();
    s.insert_message(&mut m).unwrap();
    s.set_reaction("m1", "a", "👍").unwrap();
    s.set_draft("g:x", Some("d"), 2).unwrap();
    s.ensure_conv("u:bob", 0).unwrap();
    s.insert_message(&mut row("keep", "别动", 1)).unwrap();
    let pending = |id: &str, gid: &str| PendingRec {
        envelope_id: id.into(),
        group_id: gid.into(),
        sender: "bob".into(),
        session_id: "s".into(),
        body: "b".into(),
        server_ts: 1,
    };
    s.add_pending(&pending("p1", "x")).unwrap();
    s.add_pending(&pending("p2", "other")).unwrap();
    s.delete_conversation("g:x").unwrap();
    assert_eq!(s.pending_count().unwrap(), 1, "只丢弃该群挂起的旧消息");
    assert!(s.get_conv("g:x").unwrap().is_none());
    assert!(s.get_message("m1").unwrap().is_none());
    assert!(s.reactions("m1").unwrap().is_empty());
    assert!(s.get_draft("g:x").unwrap().is_none());
    assert!(s.get_message("keep").unwrap().is_some(), "其他会话不受影响");
    // 重新出现：免打扰沿用，置顶不保留；提示只用一次
    assert!(s.ensure_conv("g:x", 0).unwrap());
    let c = s.get_conv("g:x").unwrap().unwrap();
    assert!(c.muted && !c.pinned);
    s.set_conv_flag("g:x", "muted", false).unwrap();
    s.delete_conversation("g:x").unwrap();
    s.ensure_conv("g:x", 0).unwrap();
    assert!(!s.get_conv("g:x").unwrap().unwrap().muted);
}
