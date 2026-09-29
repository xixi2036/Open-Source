//! 端到端集成测试：启动真实 Go 服务端（../server），三个 HushClient 走完主要业务流程。
//!
//! 若 `../server/cmd/hush-server` 不存在（服务端尚未完成）或本机没有 `go`，测试打印原因后直接通过。

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use hushcore::*;

const CODE: &str = "123456";
const TIMEOUT: Duration = Duration::from_secs(20);

// ---------------- 服务端进程 ----------------

struct Server {
    child: Child,
    url: String,
    log: PathBuf,
    _dir: tempfile::TempDir,
}

impl Drop for Server {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn free_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port()
}

fn start_server() -> Option<Server> {
    start_server_with(&[])
}

/// 启动服务端，`extra` 为附加命令行参数（例如 `--token-ttl 3s`）。
fn start_server_with(extra: &[&str]) -> Option<Server> {
    let server_dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../server");
    if !server_dir.join("cmd/hush-server").exists() {
        eprintln!("跳过 e2e：{} 不存在", server_dir.join("cmd/hush-server").display());
        return None;
    }
    let dir = tempfile::tempdir().unwrap();
    let bin = dir.path().join("hush-server");
    let st = Command::new("go").args(["build", "-o"]).arg(&bin).arg("./cmd/hush-server").current_dir(&server_dir).status();
    match st {
        Ok(s) if s.success() => {}
        other => {
            eprintln!("跳过 e2e：go build 失败 {other:?}");
            return None;
        }
    }
    let port = free_port();
    let log = dir.path().join("server.log");
    let logf = std::fs::File::create(&log).unwrap();
    let child = Command::new(&bin)
        .args(["--addr", &format!("127.0.0.1:{port}"), "--turn-port", "0", "--public-ip", "127.0.0.1", "--dev"])
        .args(extra)
        .arg("--data")
        .arg(dir.path().join("data"))
        .env("HUSH_DEV_CODE", CODE)
        .stdout(Stdio::from(logf.try_clone().unwrap()))
        .stderr(Stdio::from(logf))
        .spawn()
        .expect("启动 hush-server");
    let url = format!("http://127.0.0.1:{port}");
    let s = Server { child, url: url.clone(), log, _dir: dir };
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        if std::net::TcpStream::connect(("127.0.0.1", port)).is_ok() {
            break;
        }
        assert!(Instant::now() < deadline, "服务端未能启动，日志：{}", std::fs::read_to_string(&s.log).unwrap_or_default());
        std::thread::sleep(Duration::from_millis(100));
    }
    Some(s)
}

// ---------------- 客户端封装 ----------------

struct Collector(Arc<Mutex<Vec<CoreEvent>>>);

/// 模拟平台回调抛异常。
struct Panicker;

impl EventListener for Panicker {
    fn on_event(&self, _event: CoreEvent) {
        panic!("平台回调异常");
    }
}

impl EventListener for Collector {
    fn on_event(&self, event: CoreEvent) {
        self.0.lock().unwrap().push(event);
    }
}

struct TC {
    c: Arc<HushClient>,
    events: Arc<Mutex<Vec<CoreEvent>>>,
    uid: String,
    name: String,
    _dir: tempfile::TempDir,
}

fn db_key(name: &str) -> Vec<u8> {
    (0..32u8).map(|i| i.wrapping_mul(7).wrapping_add(name.len() as u8)).collect()
}

impl TC {
    async fn register(url: &str, name: &str) -> TC {
        Self::login(url, name, true).await
    }

    /// 在新的数据目录登录（`fresh=false` 表示已注册用户在另一台设备登录）。
    async fn login(url: &str, name: &str, fresh: bool) -> TC {
        let dir = tempfile::tempdir().unwrap();
        let c = HushClient::new(dir.path().to_string_lossy().to_string(), db_key(name), "android".into(), format!("{name}-device")).unwrap();
        let events = Arc::new(Mutex::new(Vec::new()));
        c.set_listener(Arc::new(Collector(events.clone())));
        let email = format!("{}@hush.test", name.to_lowercase());
        if fresh {
            c.request_code(url.into(), email.clone()).await.expect("request_code");
        }
        let acc = c.verify_code(url.into(), email, CODE.into()).await.expect("verify_code");
        assert_eq!(acc.is_new, fresh);
        assert!(c.is_logged_in());
        c.update_profile(Some(name.into()), Some(format!("我是 {name}")), None).await.unwrap();
        c.start().unwrap();
        TC { uid: acc.user_id, c, events, name: name.into(), _dir: dir }
    }

    fn has_event(&self, pred: impl Fn(&CoreEvent) -> bool) -> bool {
        self.events.lock().unwrap().iter().any(pred)
    }

    fn msgs(&self, conv: &str) -> Vec<MessageItem> {
        self.c.messages(conv.into(), None, 200).unwrap()
    }

    fn find_text(&self, conv: &str, text: &str) -> Option<MessageItem> {
        self.msgs(conv).into_iter().find(|m| matches!(&m.body, MessageBody::Text { text: t } if t == text))
    }
}

async fn eventually<T>(what: &str, mut f: impl FnMut() -> Option<T>) -> T {
    let deadline = Instant::now() + TIMEOUT;
    loop {
        if let Some(v) = f() {
            return v;
        }
        if Instant::now() > deadline {
            panic!("等待超时：{what}");
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

async fn make_friends(a: &TC, b: &TC) {
    let found = a.c.search_user_by_email(format!("{}@HUSH.test", b.name.to_lowercase())).await.unwrap().expect("按邮箱搜到用户");
    assert_eq!(found.id, b.uid);
    a.c.send_friend_request(b.uid.clone(), format!("我是 {}", a.name)).await.unwrap();
    eventually("收到好友申请事件", || {
        b.has_event(|e| matches!(e, CoreEvent::FriendRequestReceived { request } if request.from.id == a.uid)).then_some(())
    })
    .await;
    let reqs = b.c.list_friend_requests().await.unwrap();
    let r = reqs.incoming.iter().find(|r| r.from.id == a.uid).expect("incoming 申请");
    b.c.accept_friend_request(r.id.clone()).await.unwrap();
    assert!(b.c.friends().unwrap().iter().any(|p| p.id == a.uid));
    eventually("申请方好友列表更新", || a.c.friends().unwrap().iter().any(|p| p.id == b.uid).then_some(())).await;
}

fn tiny_png() -> Vec<u8> {
    let mut png = b"\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR".to_vec();
    png.extend_from_slice(&3u32.to_be_bytes());
    png.extend_from_slice(&2u32.to_be_bytes());
    png.extend_from_slice(&[8, 2, 0, 0, 0, 0, 0, 0, 0]);
    png
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn e2e_full_flow() {
    let Some(server) = start_server() else { return };
    let url = server.url.clone();
    let result = tokio::time::timeout(Duration::from_secs(240), flow(&url)).await;
    if !matches!(result, Ok(())) {
        eprintln!("---- 服务端日志 ----\n{}", std::fs::read_to_string(&server.log).unwrap_or_default());
    }
    result.expect("e2e 超时");
}

async fn flow(url: &str) {
    // ---------- 注册 ----------
    let alice = TC::register(url, "Alice").await;
    let bob = TC::register(url, "Bob").await;
    let carol = TC::register(url, "Carol").await;
    for t in [&alice, &bob, &carol] {
        eventually("WS 已连接", || (t.c.connection_state() == ConnectionState::Connected).then_some(())).await;
        assert_eq!(t.c.my_account().unwrap().profile.name, t.name);
        assert_eq!(t.c.my_qr_payload(), format!("hush-user:v1:{}", t.uid));
    }
    eprintln!("✓ 注册");

    // ---------- 加好友 ----------
    make_friends(&alice, &bob).await;
    make_friends(&alice, &carol).await;
    // ---------- C-C：好友可见邮箱，非好友不可见 ----------
    assert_eq!(alice.c.my_account().unwrap().profile.email.as_deref(), Some("alice@hush.test"));
    let bp = alice.c.get_user(bob.uid.clone()).await.unwrap();
    assert_eq!(bp.email.as_deref(), Some("bob@hush.test"), "好友可见邮箱");
    assert_eq!(alice.c.cached_profile(bob.uid.clone()).unwrap().email.as_deref(), Some("bob@hush.test"));
    assert!(alice.c.friends().unwrap().iter().all(|p| p.email.is_some()), "好友列表带邮箱");
    let cp = bob.c.get_user(carol.uid.clone()).await.unwrap();
    assert_eq!(cp.email, None, "非好友不可见邮箱");
    eprintln!("✓ 好友资料邮箱");
    eprintln!("✓ 加好友");

    // ---------- 单聊：PreKey 与 Normal ----------
    let ab = alice.c.direct_conversation_id(bob.uid.clone());
    let ba = bob.c.direct_conversation_id(alice.uid.clone());
    assert_eq!(ab, format!("u:{}", bob.uid));
    let m1 = alice.c.send_text(ab.clone(), "你好 Bob".into(), None).await.unwrap();
    assert_eq!(m1.status, MessageStatus::Sent);
    let got = eventually("Bob 收到第一条消息", || bob.find_text(&ba, "你好 Bob")).await;
    assert_eq!(got.id, m1.id);
    assert!(!got.is_outgoing);
    assert!(bob.c.debug_stats().olm_prekey_rx >= 1, "首条应为 PreKey 消息");
    assert!(alice.c.debug_stats().olm_prekey_tx >= 1);
    // 落盘为密文
    let raw = bob.c.debug_raw_message_body(&got.id).unwrap();
    assert!(!String::from_utf8_lossy(&raw).contains("你好 Bob"));

    // 送达回执（Bob 自动发出，经 Normal 消息）
    eventually("送达回执", || {
        alice.msgs(&ab).iter().find(|m| m.id == m1.id).filter(|m| m.status == MessageStatus::Delivered).map(|_| ())
    })
    .await;
    assert!(alice.c.debug_stats().olm_normal_rx >= 1, "回执应为 Normal 消息");

    let reply = bob.c.send_text(ba.clone(), "你好 Alice".into(), Some(got.id.clone())).await.unwrap();
    assert_eq!(reply.quote.as_ref().unwrap().id, m1.id);
    let r2 = eventually("Alice 收到回复", || alice.find_text(&ab, "你好 Alice")).await;
    assert_eq!(r2.quote.unwrap().text, "你好 Bob");
    alice.c.send_text(ab.clone(), "第二条".into(), None).await.unwrap();
    eventually("Bob 收到第二条", || bob.find_text(&ba, "第二条")).await;
    assert!(bob.c.debug_stats().olm_normal_rx >= 1, "后续消息应为 Normal");
    // 至少一次投递：同一信封重投必须幂等（不能出现无法解密占位、不能触发 session_reset）
    let env = bob.c.debug_last_envelope().expect("Bob 处理过信封");
    let before = bob.msgs(&ba).len();
    assert!(bob.c.debug_redeliver(env.clone()).await);
    assert!(bob.c.debug_redeliver(env).await);
    assert_eq!(bob.msgs(&ba).len(), before);
    assert_eq!(bob.c.debug_stats().undecryptable, 0);
    let conv = bob.c.conversation(ba.clone()).unwrap().unwrap();
    assert_eq!(conv.title, "Alice");
    assert!(conv.unread_count >= 2);
    eprintln!("✓ 单聊 PreKey/Normal（stats alice={:?} bob={:?}）", alice.c.debug_stats(), bob.c.debug_stats());

    // ---------- 已读回执 ----------
    bob.c.mark_read(ba.clone()).await.unwrap();
    assert_eq!(bob.c.conversation(ba.clone()).unwrap().unwrap().unread_count, 0);
    eventually("已读回执", || {
        alice.msgs(&ab).iter().find(|m| m.id == m1.id).filter(|m| m.status == MessageStatus::Read).map(|_| ())
    })
    .await;
    eprintln!("✓ 送达/已读回执");

    // ---------- 输入状态 ----------
    alice.c.set_typing(ab.clone(), true).await.unwrap();
    eventually("正在输入", || {
        bob.has_event(|e| matches!(e, CoreEvent::Typing { conv_id, typing: true, .. } if *conv_id == ba)).then_some(())
    })
    .await;

    // ---------- 附件 ----------
    let payload: Vec<u8> = (0..300_000u32).map(|i| (i.wrapping_mul(31) % 253) as u8).collect();
    let fpath = alice._dir.path().join("report.bin");
    std::fs::write(&fpath, &payload).unwrap();
    let meta = AttachmentMeta { file_name: Some("报告.bin".into()), width: 0, height: 0, duration_ms: 0, thumbnail: Some(vec![1, 2, 3]) };
    let am = alice
        .c
        .send_attachment(ab.clone(), fpath.to_string_lossy().into(), "application/octet-stream".into(), AttachmentKind::File, Some("请查收".into()), meta)
        .await
        .unwrap();
    assert_eq!(am.status, MessageStatus::Sent);
    let bm = eventually("Bob 收到附件", || bob.msgs(&ba).into_iter().find(|m| m.id == am.id)).await;
    match &bm.body {
        MessageBody::Attachments { items, caption } => {
            assert_eq!(caption.as_deref(), Some("请查收"));
            assert_eq!(items[0].file_name, "报告.bin");
            assert_eq!(items[0].size, payload.len() as u64);
            assert_eq!(items[0].thumbnail.as_deref(), Some(&[1u8, 2, 3][..]));
            assert!(items[0].local_path.is_none());
        }
        other => panic!("期望附件，得到 {other:?}"),
    }
    let path = bob.c.download_attachment(bm.id.clone(), 0).await.unwrap();
    assert_eq!(std::fs::read(&path).unwrap(), payload);
    let again = bob.c.download_attachment(bm.id.clone(), 0).await.unwrap();
    assert_eq!(again, path, "第二次走缓存");

    // 20MB 附件经真实服务端往返（v2 分块流式加密，流式上传 / 下载）
    let big: Vec<u8> = (0..20 * 1024 * 1024 + 77u32).map(|i| (i.wrapping_mul(131) >> 3) as u8).collect();
    let bpath = alice._dir.path().join("big.bin");
    std::fs::write(&bpath, &big).unwrap();
    let meta = AttachmentMeta { file_name: Some("big.bin".into()), ..Default::default() };
    let bam = alice
        .c
        .send_attachment(ab.clone(), bpath.to_string_lossy().into(), "application/octet-stream".into(), AttachmentKind::File, None, meta)
        .await
        .unwrap();
    let bbm = eventually("Bob 收到大附件", || bob.msgs(&ba).into_iter().find(|m| m.id == bam.id)).await;
    let bp2 = bob.c.download_attachment(bbm.id.clone(), 0).await.unwrap();
    assert!(std::fs::read(&bp2).unwrap() == big, "20MB 附件内容一致");
    drop(big);
    eprintln!("✓ 附件收发（含 20MB）");

    // ---------- C-A：sort_ts 翻页 ----------
    let all = bob.msgs(&ba);
    assert!(all.windows(2).all(|w| w[0].sort_ts <= w[1].sort_ts), "按 sort_ts 升序");
    let last = all.last().unwrap();
    let older = bob.c.messages(ba.clone(), Some(last.sort_ts), 500).unwrap();
    assert!(older.iter().all(|m| m.sort_ts < last.sort_ts) && !older.iter().any(|m| m.id == last.id));
    assert_eq!(older.len() + all.iter().filter(|m| m.sort_ts == last.sort_ts).count(), all.len(), "以 sort_ts 为游标不漏不重");
    eprintln!("✓ sort_ts 翻页");

    // ---------- 安全码 ----------
    let sa = alice.c.safety_number(bob.uid.clone()).await.unwrap();
    let sb = bob.c.safety_number(alice.uid.clone()).await.unwrap();
    assert_eq!(sa.digits, sb.digits);
    assert_eq!(sa.digits.len(), 60);
    assert!(!sa.verified);
    assert!(bob.c.verify_safety_qr(alice.uid.clone(), sa.qr_payload.clone()).unwrap());
    assert!(bob.c.safety_number(alice.uid.clone()).await.unwrap().verified);
    assert!(!bob.c.verify_safety_qr(alice.uid.clone(), "hush-sn:v1:000".into()).unwrap());
    eprintln!("✓ 安全码");

    // ---------- 撤回 ----------
    let del = alice.c.send_text(ab.clone(), "这条要撤回".into(), None).await.unwrap();
    eventually("Bob 收到待撤回消息", || bob.find_text(&ba, "这条要撤回")).await;
    alice.c.delete_for_everyone(del.id.clone()).await.unwrap();
    eventually("Bob 端撤回", || bob.msgs(&ba).into_iter().find(|m| m.id == del.id && m.deleted)).await;
    assert!(alice.msgs(&ab).iter().any(|m| m.id == del.id && m.deleted));
    eprintln!("✓ 撤回");

    // ---------- 回应 ----------
    bob.c.react(m1.id.clone(), "👍".into(), false).await.unwrap();
    eventually("Alice 看到回应", || {
        alice.msgs(&ab).into_iter().find(|m| m.id == m1.id && m.reactions.iter().any(|r| r.emoji == "👍" && r.user_ids.contains(&bob.uid)))
    })
    .await;
    bob.c.react(m1.id.clone(), "👍".into(), true).await.unwrap();
    eventually("Alice 看到取消回应", || alice.msgs(&ab).into_iter().find(|m| m.id == m1.id && m.reactions.is_empty())).await;
    eprintln!("✓ 回应");

    // ---------- 建群 + 群消息 ----------
    let g = alice.c.create_group("三人小组".into(), vec![bob.uid.clone(), carol.uid.clone()]).await.unwrap();
    assert_eq!(g.members.len(), 3);
    let gc = format!("g:{}", g.id);
    for t in [&bob, &carol] {
        eventually("成员收到建群", || t.c.group_info(g.id.clone()).unwrap().filter(|x| x.active)).await;
    }
    alice.c.send_text(gc.clone(), "大家好".into(), None).await.unwrap();
    for t in [&bob, &carol] {
        eventually("成员收到群消息", || t.find_text(&gc, "大家好")).await;
    }
    let s1 = alice.c.debug_outbound_session_id(&g.id).expect("Alice 出站会话");
    assert!(carol.c.debug_has_inbound_session(&alice.uid, &s1));
    bob.c.send_text(gc.clone(), "Bob 在群里".into(), None).await.unwrap();
    for t in [&alice, &carol] {
        eventually("群成员收到 Bob 的消息", || t.find_text(&gc, "Bob 在群里")).await;
    }
    assert_eq!(bob.c.conversation(gc.clone()).unwrap().unwrap().title, "三人小组");
    eprintln!("✓ 建群与群消息（Megolm）");

    // 群内已读回执经 Olm 发回原发送者
    let gm = alice.c.send_text(gc.clone(), "请已读".into(), None).await.unwrap();
    eventually("Bob 收到", || bob.find_text(&gc, "请已读")).await;
    bob.c.mark_read(gc.clone()).await.unwrap();
    eventually("群消息已读", || alice.msgs(&gc).into_iter().find(|m| m.id == gm.id && m.status == MessageStatus::Read)).await;

    // ---------- 踢人 → 轮换 ----------
    alice.c.remove_member(g.id.clone(), carol.uid.clone()).await.unwrap();
    eventually("Carol 被移出", || carol.c.group_info(g.id.clone()).unwrap().filter(|x| !x.active)).await;
    eventually("Bob 看到成员变化", || bob.c.group_info(g.id.clone()).unwrap().filter(|x| x.members.len() == 2)).await;
    alice.c.send_text(gc.clone(), "踢人之后".into(), None).await.unwrap();
    eventually("Bob 收到踢人后的消息", || bob.find_text(&gc, "踢人之后")).await;
    let s2 = alice.c.debug_outbound_session_id(&g.id).expect("新出站会话");
    assert_ne!(s1, s2, "踢人后必须轮换 Megolm 会话");
    assert!(bob.c.debug_has_inbound_session(&alice.uid, &s2));
    tokio::time::sleep(Duration::from_millis(800)).await;
    assert!(!carol.c.debug_has_inbound_session(&alice.uid, &s2), "被踢者不应拿到新会话密钥");
    assert!(carol.find_text(&gc, "踢人之后").is_none(), "被踢者不应看到新消息");
    assert!(carol.c.group_info(g.id.clone()).unwrap().map(|x| !x.active).unwrap_or(false));
    eprintln!("✓ 踢人后轮换密钥（{s1} → {s2}）");

    // ---------- 朋友圈 ----------
    let img = alice._dir.path().join("sky.png");
    std::fs::write(&img, tiny_png()).unwrap();
    let mo = alice.c.post_moment("今天天气不错".into(), vec![img.to_string_lossy().into()], Visibility::default()).await.unwrap();
    assert_eq!(mo.media.len(), 1);
    assert_eq!((mo.media[0].width, mo.media[0].height), (3, 2));
    let feed = bob.c.moments_feed(None).await.unwrap();
    let fm = feed.iter().find(|m| m.id == mo.id).expect("Bob 的动态流里有 Alice 的动态");
    assert_eq!(fm.author.id, alice.uid);
    let media_path = bob.c.fetch_media(fm.media[0].attachment_id.clone()).await.unwrap();
    assert_eq!(std::fs::read(media_path).unwrap(), tiny_png());
    bob.c.like_moment(mo.id.clone(), true).await.unwrap();
    let cm = bob.c.comment_moment(mo.id.clone(), "确实".into(), None).await.unwrap();
    assert_eq!(cm.author.id, bob.uid);
    eventually("Alice 收到互动通知", || {
        alice
            .has_event(|e| matches!(e, CoreEvent::MomentNotification { notification } if notification.kind == MomentNotificationKind::Comment))
            .then_some(())
    })
    .await;
    let mine = alice.c.user_moments(alice.uid.clone(), None).await.unwrap();
    let m = mine.iter().find(|m| m.id == mo.id).unwrap();
    assert!(m.likes.iter().any(|p| p.id == bob.uid));
    assert!(m.comments.iter().any(|c| c.text == "确实"));
    let notes = alice.c.moment_notifications().await.unwrap();
    assert!(notes.iter().any(|n| n.kind == MomentNotificationKind::Like && n.from.id == bob.uid));
    // C-D：通知点进单条动态；删除后不可见
    let note = notes.iter().find(|n| n.moment_id == mo.id).unwrap();
    let single = alice.c.moment(note.moment_id.clone()).await.unwrap();
    assert_eq!(single.id, mo.id);
    assert!(single.comments.iter().any(|c| c.text == "确实"));
    assert_eq!(bob.c.moment(mo.id.clone()).await.unwrap().text, "今天天气不错");
    let tmp = alice.c.post_moment("马上删除".into(), vec![], Visibility::default()).await.unwrap();
    alice.c.delete_moment(tmp.id.clone()).await.unwrap();
    assert!(matches!(bob.c.moment(tmp.id.clone()).await, Err(HushError::NotFound { .. })), "已删除 → NotFound");
    assert!(matches!(bob.c.moment("00000000-0000-0000-0000-000000000000".into()).await, Err(HushError::NotFound { .. })));
    eprintln!("✓ 朋友圈发帖/点赞/评论/单条动态");

    // ---------- 通话信令 ----------
    // 测试服务端以 --turn-port 0 启动（不启用 TURN）：按 API_NOTES 返回空 urls，凭据照常下发
    let turn = alice.c.turn_credentials().await.unwrap();
    assert!(turn.urls.is_empty(), "未启用 TURN 时不应下发地址: {:?}", turn.urls);
    assert!(!turn.username.is_empty() && !turn.credential.is_empty());
    let call_id = "call-1".to_string();
    let sig = |action: CallAction, sdp: Option<&str>| CallSignal {
        call_id: call_id.clone(),
        action,
        video: false,
        sdp: sdp.map(|s| s.to_string()),
        candidates: vec![],
    };
    alice.c.send_call_signal(bob.uid.clone(), sig(CallAction::Offer, Some("v=0 offer"))).await.unwrap();
    eventually("Bob 收到 offer", || {
        bob.has_event(|e| {
            matches!(e, CoreEvent::CallSignalReceived { from_user_id, signal }
                if *from_user_id == alice.uid && signal.action == CallAction::Offer && signal.sdp.as_deref() == Some("v=0 offer"))
        })
        .then_some(())
    })
    .await;
    bob.c.send_call_signal(alice.uid.clone(), sig(CallAction::Answer, Some("v=0 answer"))).await.unwrap();
    let ice = CallSignal {
        call_id: call_id.clone(),
        action: CallAction::Ice,
        video: false,
        sdp: None,
        candidates: vec![IceCandidate { sdp: "candidate:1 1 udp 1 127.0.0.1 5000 typ host".into(), sdp_mid: Some("0".into()), sdp_mline_index: Some(0) }],
    };
    bob.c.send_call_signal(alice.uid.clone(), ice).await.unwrap();
    eventually("Alice 收到 answer 与 ice", || {
        (alice.has_event(|e| matches!(e, CoreEvent::CallSignalReceived { signal, .. } if signal.action == CallAction::Answer))
            && alice.has_event(|e| matches!(e, CoreEvent::CallSignalReceived { signal, .. } if signal.action == CallAction::Ice && signal.candidates.len() == 1)))
        .then_some(())
    })
    .await;
    alice.c.send_call_signal(bob.uid.clone(), sig(CallAction::Hangup, None)).await.unwrap();
    for (t, conv, outgoing) in [(&alice, &ab, true), (&bob, &ba, false)] {
        eventually("通话记录", || {
            t.msgs(conv).into_iter().find(|m| {
                matches!(m.body, MessageBody::CallLog { outgoing: o, missed: false, reason: CallEndReason::Completed, .. } if o == outgoing)
            })
        })
        .await;
    }
    eprintln!("✓ 通话信令往返与通话记录");

    // ---------- 阅后即焚 ----------
    alice.c.set_disappearing(ab.clone(), 1).await.unwrap();
    eventually("Bob 同步计时器", || bob.c.conversation(ba.clone()).unwrap().filter(|c| c.disappearing_secs == 1)).await;
    let eph = alice.c.send_text(ab.clone(), "阅后即焚".into(), None).await.unwrap();
    assert_eq!(eph.expire_secs, 1);
    eventually("Bob 收到阅后即焚消息", || bob.find_text(&ba, "阅后即焚")).await;
    eventually("发送方到期删除", || alice.find_text(&ab, "阅后即焚").is_none().then_some(())).await;
    assert!(bob.find_text(&ba, "阅后即焚").is_some(), "接收方未读前不计时");
    bob.c.mark_read(ba.clone()).await.unwrap();
    eventually("接收方已读后到期删除", || bob.find_text(&ba, "阅后即焚").is_none().then_some(())).await;
    // 事件经专门线程异步投递，需等待
    eventually("MessageRemoved 事件", || {
        bob.has_event(|e| matches!(e, CoreEvent::MessageRemoved { message_id, .. } if *message_id == eph.id)).then_some(())
    })
    .await;
    eprintln!("✓ 阅后即焚");

    // ---------- 本地搜索 / 置顶 ----------
    let found = alice.c.search_messages("bob 在群".into()).unwrap();
    assert!(found.iter().any(|m| m.conv_id == gc));
    alice.c.set_pinned(gc.clone(), true).unwrap();
    assert_eq!(alice.c.conversations(false).unwrap()[0].id, gc);

    assert_eq!(alice.c.debug_stats().undecryptable + bob.c.debug_stats().undecryptable, 0);
    assert_eq!(bob.c.debug_pending_count(), 0);
    for t in [&alice, &bob, &carol] {
        t.c.stop();
    }
    eprintln!("✓ 全流程通过");
}

/// 离线投递、换设备登录（身份变更 → 重建会话）、旧设备被挤下线、拉黑、本地库重开。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn e2e_relogin_offline_block() {
    let Some(server) = start_server() else { return };
    let url = server.url.clone();
    let result = tokio::time::timeout(Duration::from_secs(180), flow2(&url)).await;
    if !matches!(result, Ok(())) {
        eprintln!("---- 服务端日志 ----\n{}", std::fs::read_to_string(&server.log).unwrap_or_default());
    }
    result.expect("e2e 超时");
}

async fn flow2(url: &str) {
    let alice = TC::register(url, "Dora").await;
    let bob = TC::register(url, "Evan").await;
    make_friends(&alice, &bob).await;
    let ab = format!("u:{}", bob.uid);
    let ba = format!("u:{}", alice.uid);

    // ---------- 离线投递 ----------
    alice.c.send_text(ab.clone(), "在线".into(), None).await.unwrap();
    eventually("在线消息", || bob.find_text(&ba, "在线")).await;
    let added_backlog = |t: &TC, text: &str| -> Option<bool> {
        t.events.lock().unwrap().iter().find_map(|e| match e {
            CoreEvent::MessageAdded { message, backlog, .. } if matches!(&message.body, MessageBody::Text { text: x } if x == text) => Some(*backlog),
            _ => None,
        })
    };
    assert!(!eventually("在线消息事件", || added_backlog(&bob, "在线")).await, "实时推送 backlog=false");
    bob.c.stop();
    assert_eq!(bob.c.connection_state(), ConnectionState::Disconnected);
    alice.c.send_text(ab.clone(), "离线时发的".into(), None).await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(bob.find_text(&ba, "离线时发的").is_none());
    bob.c.start().unwrap();
    eventually("重连后收到离线消息", || bob.find_text(&ba, "离线时发的")).await;
    assert!(eventually("离线消息事件", || added_backlog(&bob, "离线时发的")).await, "积压 backlog=true");
    eventually("连接后", || (bob.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    tokio::time::sleep(Duration::from_millis(300)).await; // 等 queue_empty
    alice.c.send_text(ab.clone(), "重连后实时".into(), None).await.unwrap();
    assert!(!eventually("重连后实时消息事件", || added_backlog(&bob, "重连后实时")).await, "queue_empty 之后 backlog=false");
    eprintln!("✓ 离线投递（backlog 标记）");

    // ---------- HTTP 拉取兜底 ----------
    bob.c.stop();
    alice.c.send_text(ab.clone(), "轮询拉取".into(), None).await.unwrap();
    let n = bob.c.sync_now().await.unwrap();
    assert!(n >= 1);
    assert!(bob.find_text(&ba, "轮询拉取").is_some());
    assert!(eventually("补拉消息事件", || added_backlog(&bob, "轮询拉取")).await, "HTTP 补拉 backlog=true");
    eprintln!("✓ HTTP 拉取兜底");

    // ---------- 本地库重开 ----------
    {
        let dir = bob._dir.path().to_string_lossy().to_string();
        let reopened = HushClient::new(dir.clone(), db_key("Evan"), "android".into(), "x".into()).unwrap();
        assert!(reopened.is_logged_in());
        assert_eq!(reopened.my_account().unwrap().user_id, bob.uid);
        let texts: Vec<_> = reopened.messages(ba.clone(), None, 50).unwrap();
        assert!(texts.iter().any(|m| matches!(&m.body, MessageBody::Text { text } if text == "离线时发的")));
        let err = HushClient::new(dir, vec![9u8; 32], "android".into(), "x".into()).err().expect("错误库密钥必须拒绝");
        assert!(matches!(err, HushError::KeyMismatch { .. }), "{err:?}");
    }
    bob.c.start().unwrap();
    eprintln!("✓ 本地库重开 / 错误密钥拒绝");

    // ---------- 换设备登录 ----------
    let old_safety = alice.c.safety_number(bob.uid.clone()).await.unwrap();
    let bob2 = TC::login(url, "Evan", false).await;
    assert_eq!(bob2.uid, bob.uid);
    eventually("旧设备被挤下线", || {
        bob.has_event(|e| matches!(e, CoreEvent::LoggedOut { .. })).then_some(())
    })
    .await;
    assert!(!bob.c.is_logged_in());
    eventually("Alice 收到 KeyChanged", || {
        alice.has_event(|e| matches!(e, CoreEvent::KeyChanged { user_id } if *user_id == bob.uid)).then_some(())
    })
    .await;
    eventually("安全码变更提示", || {
        alice.msgs(&ab).into_iter().find(|m| matches!(&m.body, MessageBody::System { text } if text.contains("安全码已变更")))
    })
    .await;
    let new_safety = alice.c.safety_number(bob.uid.clone()).await.unwrap();
    assert_ne!(old_safety.digits, new_safety.digits);
    let prekeys_before = bob2.c.debug_stats().olm_prekey_rx;
    alice.c.send_text(ab.clone(), "新设备你好".into(), None).await.unwrap();
    eventually("新设备收到消息", || bob2.find_text(&ba, "新设备你好")).await;
    assert!(bob2.c.debug_stats().olm_prekey_rx > prekeys_before, "身份变更后应重新建立会话");
    bob2.c.send_text(ba.clone(), "新设备回复".into(), None).await.unwrap();
    eventually("Alice 收到新设备回复", || alice.find_text(&ab, "新设备回复")).await;
    eprintln!("✓ 换设备登录：旧设备登出、KeyChanged、重建会话");

    // ---------- 拉黑 ----------
    bob2.c.block(alice.uid.clone()).await.unwrap();
    assert_eq!(bob2.c.blocked().unwrap(), vec![alice.uid.clone()]);
    let err = alice.c.send_text(ab.clone(), "被拉黑后".into(), None).await.unwrap_err();
    assert!(err.is_server_code("blocked"), "{err:?}");
    let failed = alice.find_text(&ab, "被拉黑后").unwrap();
    assert_eq!(failed.status, MessageStatus::Failed);
    bob2.c.unblock(alice.uid.clone()).await.unwrap();
    let resent = alice.c.resend(failed.id.clone()).await.unwrap();
    assert_eq!(resent.status, MessageStatus::Sent);
    eventually("重发后送达", || bob2.find_text(&ba, "被拉黑后")).await;
    eprintln!("✓ 拉黑 / 失败重发");

    // ---------- 删除好友 ----------
    alice.c.remove_friend(bob.uid.clone()).await.unwrap();
    eventually("对方好友列表更新", || (!bob2.c.friends().unwrap().iter().any(|p| p.id == alice.uid)).then_some(())).await;
    assert!(alice.c.send_text(ab.clone(), "已不是好友".into(), None).await.unwrap_err().is_server_code("not_friends"));

    // ---------- 退出登录 ----------
    alice.c.logout().await.unwrap();
    assert!(!alice.c.is_logged_in());
    assert!(alice.c.conversations(false).unwrap().is_empty());
    bob2.c.stop();
    eprintln!("✓ 删除好友 / 退出登录");
}

/// 健壮性：超限帧退回 HTTP 拉取等（审查修复的回归测试）。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn e2e_resilience() {
    let Some(server) = start_server() else { return };
    let url = server.url.clone();
    let result = tokio::time::timeout(Duration::from_secs(180), flow3(&url)).await;
    if !matches!(result, Ok(())) {
        eprintln!("---- 服务端日志 ----\n{}", std::fs::read_to_string(&server.log).unwrap_or_default());
    }
    result.expect("e2e 超时");
}

async fn flow3(url: &str) {
    let fay = TC::register(url, "Fay").await;
    let gus = TC::register(url, "Gus").await;
    make_friends(&fay, &gus).await;
    let fg = format!("u:{}", gus.uid);
    let gf = format!("u:{}", fay.uid);
    fay.c.send_text(fg.clone(), "热身".into(), None).await.unwrap();
    eventually("热身消息", || gus.find_text(&gf, "热身")).await;

    // ---------- 超限帧：不能无限重连，应退回 HTTP 拉取处理掉 ----------
    gus.c.stop();
    gus.c.debug_set_ws_max_frame(4096);
    gus.c.start().unwrap();
    eventually("Gus 重新连上", || (gus.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    let big: String = "大".repeat(8000);
    fay.c.send_text(fg.clone(), big.clone(), None).await.unwrap();
    eventually("超限帧经 HTTP 拉取送达", || gus.find_text(&gf, &big)).await;
    assert!(gus.c.debug_stats().ws_oversized >= 1, "应走过超限帧分支");
    fay.c.send_text(fg.clone(), "超限之后".into(), None).await.unwrap();
    eventually("之后的小消息照常经 WS 送达", || gus.find_text(&gf, "超限之后")).await;
    assert_eq!(gus.c.debug_stats().undecryptable, 0);
    gus.c.stop();
    gus.c.debug_set_ws_max_frame(1 << 20);
    gus.c.start().unwrap();
    eprintln!("✓ 超限帧退回 HTTP 拉取");

    // ---------- 平台回调 panic 不影响核心；WS 任务退出后 start() 重新拉起 ----------
    gus.c.set_listener(Arc::new(Panicker));
    fay.c.send_text(fg.clone(), "回调会 panic".into(), None).await.unwrap();
    eventually("回调 panic 时照常收消息", || gus.find_text(&gf, "回调会 panic")).await;
    fay.c.send_text(fg.clone(), "再来一条".into(), None).await.unwrap();
    eventually("WS 未被 panic 终止", || gus.find_text(&gf, "再来一条")).await;
    gus.c.set_listener(Arc::new(Collector(gus.events.clone())));
    gus.c.debug_kill_ws_task();
    tokio::time::sleep(Duration::from_millis(200)).await;
    gus.c.start().unwrap();
    fay.c.send_text(fg.clone(), "重新拉起".into(), None).await.unwrap();
    eventually("start() 重新拉起 WS 后收到消息", || gus.find_text(&gf, "重新拉起")).await;
    eprintln!("✓ 回调 panic 隔离 / start() 重启 WS");

    // ---------- 4000（被同账号新连接替换）：不登出、不清密钥；重试一次，再被替换则停止重连 ----------
    let token = gus.c.debug_token().unwrap();
    let fb_before = gus.c.debug_fallback_meta();
    let intruder1 = hushcore::ws::connect(url, &token).await.expect("第二条连接");
    eventually("Gus 被替换后断开", || (gus.c.connection_state() != ConnectionState::Connected).then_some(())).await;
    // Gus 约 3 秒后重试一次，挤掉 intruder1（intruder1 收到 4000）
    assert_eq!(wait_close_code(intruder1).await, Some(4000), "Gus 重试后应挤掉另一条连接");
    eventually("Gus 重试后恢复连接", || (gus.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    let intruder2 = hushcore::ws::connect(url, &token).await.expect("第三条连接");
    eventually("Gus 再次被替换", || (gus.c.connection_state() != ConnectionState::Connected).then_some(())).await;
    tokio::time::sleep(Duration::from_secs(5)).await;
    assert_eq!(gus.c.connection_state(), ConnectionState::Disconnected, "第二次 4000 后应停止重连");
    assert!(gus.c.is_logged_in(), "4000 不能登出");
    assert_eq!(gus.c.debug_fallback_meta(), fb_before, "4000 不能清除密钥");
    assert!(!gus.has_event(|e| matches!(e, CoreEvent::LoggedOut { .. })), "4000 不发 LoggedOut");
    drop(intruder2);
    gus.c.start().unwrap();
    eventually("start() 后重新连上", || (gus.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    fay.c.send_text(fg.clone(), "被替换之后".into(), None).await.unwrap();
    eventually("被替换后仍能解密收消息", || gus.find_text(&gf, "被替换之后")).await;
    eprintln!("✓ 4000 只断开不登出");

    // ---------- fallback key 定期轮换 ----------
    let (fb1, _, used) = gus.c.debug_fallback_meta().expect("登录时记录了 fallback");
    assert!(!used);
    gus.c.debug_replenish().await.unwrap();
    assert_eq!(gus.c.debug_fallback_meta().unwrap().0, fb1, "未到期不轮换");
    gus.c.debug_age_fallback(8 * 24 * 3600 * 1000);
    gus.c.debug_replenish().await.unwrap();
    let (fb2, created, used) = gus.c.debug_fallback_meta().unwrap();
    assert_ne!(fb1, fb2, "满 7 天应轮换");
    assert!(!used && created > 0);
    fay.c.send_text(fg.clone(), "轮换 fallback 之后".into(), None).await.unwrap();
    eventually("轮换后照常收发", || gus.find_text(&gf, "轮换 fallback 之后")).await;
    eprintln!("✓ fallback key 轮换");

    // ---------- 同一信封被 WS 与 sync_now 并发处理：只处理一次，都 ack ----------
    gus.c.stop();
    fay.c.send_text(fg.clone(), "并发处理".into(), None).await.unwrap();
    let envs = eventually_async_envs(&gus).await;
    let env = envs.into_iter().last().unwrap();
    let before = gus.msgs(&gf).len();
    let rs = futures_join_all((0..4).map(|_| {
        let c = gus.c.clone();
        let e = env.clone();
        tokio::spawn(async move { c.debug_redeliver(e).await })
    }))
    .await;
    assert!(rs.iter().all(|r| *r), "并发重投都应 ack: {rs:?}");
    assert_eq!(gus.msgs(&gf).len(), before + 1, "只落库一条");
    assert!(gus.find_text(&gf, "并发处理").is_some());
    assert_eq!(gus.c.debug_stats().undecryptable, 0, "并发重复解密不能产生无法解密占位");
    let (a, b) = tokio::join!(gus.c.sync_now(), gus.c.sync_now());
    a.unwrap();
    b.unwrap();
    assert_eq!(gus.c.debug_stats().undecryptable, 0);
    gus.c.start().unwrap();
    eprintln!("✓ 并发处理同一信封");

    // ---------- 网络类失败未 ack 的信封：短暂延迟后自动重新拉取，不等重连 ----------
    let ivy = TC::register(url, "Ivy").await;
    make_friends(&fay, &ivy).await;
    eventually("Ivy 已连接", || (ivy.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    ivy.c.debug_fail_identity_fetch(1);
    let fi = format!("u:{}", ivy.uid);
    let iv = format!("u:{}", fay.uid);
    fay.c.send_text(fi.clone(), "首条 PreKey".into(), None).await.unwrap();
    eventually("获取身份失败后自动重试送达", || ivy.find_text(&iv, "首条 PreKey")).await;
    assert_eq!(ivy.c.debug_stats().undecryptable, 0);
    eprintln!("✓ 未 ack 信封自动重投");

    // ---------- Megolm 轮换不会被并发写回覆盖 ----------
    let hal = TC::register(url, "Hal").await;
    make_friends(&fay, &hal).await;
    let g = fay.c.create_group("轮换组".into(), vec![gus.uid.clone(), hal.uid.clone()]).await.unwrap();
    let gc = format!("g:{}", g.id);
    for t in [&gus, &hal] {
        eventually("成员收到建群", || t.c.group_info(g.id.clone()).unwrap().filter(|x| x.active)).await;
    }
    fay.c.send_text(gc.clone(), "第一轮".into(), None).await.unwrap();
    eventually("Hal 收到第一轮", || hal.find_text(&gc, "第一轮")).await;
    fay.c.remove_member(g.id.clone(), hal.uid.clone()).await.unwrap();
    fay.c.send_text(gc.clone(), "第二轮".into(), None).await.unwrap();
    eventually("Gus 收到第二轮", || gus.find_text(&gc, "第二轮")).await;
    let s2 = fay.c.debug_outbound_session_id(&g.id).unwrap();
    // 模拟竞态：旧会话（已分发给被移出的 Hal）被写回
    assert!(fay.c.debug_add_shared_with(&g.id, &hal.uid));
    fay.c.send_text(gc.clone(), "第三轮".into(), None).await.unwrap();
    eventually("Gus 收到第三轮", || gus.find_text(&gc, "第三轮")).await;
    let s3 = fay.c.debug_outbound_session_id(&g.id).unwrap();
    assert_ne!(s2, s3, "已分发名单含非成员时必须轮换");
    assert!(!hal.c.debug_has_inbound_session(&fay.uid, &s3));
    eprintln!("✓ 出站会话含已移出成员时强制轮换");

    // ---------- room_key 发送失败时重试一次 ----------
    let g2 = fay.c.create_group("重试组".into(), vec![gus.uid.clone()]).await.unwrap();
    let gc2 = format!("g:{}", g2.id);
    eventually("Gus 收到建群", || gus.c.group_info(g2.id.clone()).unwrap().filter(|x| x.active)).await;
    fay.c.debug_fail_room_key_posts(1);
    fay.c.send_text(gc2.clone(), "room_key 首次发送失败".into(), None).await.unwrap();
    eventually("重试后 Gus 能解密", || gus.find_text(&gc2, "room_key 首次发送失败")).await;
    assert_eq!(gus.c.debug_pending_count(), 0);
    eprintln!("✓ room_key 发送失败重试");

    for t in [&fay, &gus, &hal, &ivy] {
        t.c.stop();
    }
}

/// C-G：令牌过期（服务端 --token-ttl 很短；令牌从创建起算，节流下测试期间不会刷新）后，REST 401 与 WS 握手 401 都清除登录态并发出 LoggedOut(session_expired)。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn e2e_session_expired() {
    let Some(server) = start_server_with(&["--token-ttl", "6s"]) else { return };
    let url = server.url.clone();
    let result = tokio::time::timeout(Duration::from_secs(90), flow_expired(&url)).await;
    if !matches!(result, Ok(())) {
        eprintln!("---- 服务端日志 ----\n{}", std::fs::read_to_string(&server.log).unwrap_or_default());
    }
    result.expect("e2e 超时");
}

async fn quick_login(url: &str, name: &str) -> TC {
    let dir = tempfile::tempdir().unwrap();
    let c = HushClient::new(dir.path().to_string_lossy().to_string(), db_key(name), "android".into(), "d".into()).unwrap();
    let events = Arc::new(Mutex::new(Vec::new()));
    c.set_listener(Arc::new(Collector(events.clone())));
    let email = format!("{}@hush.test", name.to_lowercase());
    c.request_code(url.into(), email.clone()).await.unwrap();
    let acc = c.verify_code(url.into(), email, CODE.into()).await.unwrap();
    TC { uid: acc.user_id, c, events, name: name.into(), _dir: dir }
}

async fn flow_expired(url: &str) {
    let expired = |t: &TC| t.has_event(|e| matches!(e, CoreEvent::LoggedOut { reason } if reason == "session_expired"));

    // REST：令牌过期后任意接口 401
    let kim = quick_login(url, "Kim").await;
    kim.c.get_user(kim.uid.clone()).await.expect("令牌有效期内可用");
    tokio::time::sleep(Duration::from_secs(7)).await;
    let err = kim.c.get_user(kim.uid.clone()).await.unwrap_err();
    assert!(err.is_server_code("unauthorized"), "{err:?}");
    assert!(!kim.c.is_logged_in(), "401 后清除登录态");
    eventually("LoggedOut(session_expired)", || expired(&kim).then_some(())).await;
    assert!(matches!(kim.c.start(), Err(HushError::NotLoggedIn)));
    eprintln!("✓ REST 401 → session_expired");

    // WS 握手 401
    let lee = quick_login(url, "Lee").await;
    lee.c.start().unwrap();
    eventually("WS 已连接", || (lee.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    lee.c.stop();
    tokio::time::sleep(Duration::from_secs(7)).await;
    lee.c.start().unwrap();
    eventually("握手 401 → LoggedOut(session_expired)", || expired(&lee).then_some(())).await;
    assert!(!lee.c.is_logged_in());
    assert_eq!(lee.c.connection_state(), ConnectionState::Disconnected);
    let n = lee.events.lock().unwrap().iter().filter(|e| matches!(e, CoreEvent::LoggedOut { .. })).count();
    assert_eq!(n, 1, "只发一次 LoggedOut");
    eprintln!("✓ WS 握手 401 → session_expired");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn e2e_v3_polish() {
    let Some(server) = start_server() else { return };
    let url = server.url.clone();
    let result = tokio::time::timeout(Duration::from_secs(180), flow_v3(&url)).await;
    if !matches!(result, Ok(())) {
        eprintln!("---- 服务端日志 ----\n{}", std::fs::read_to_string(&server.log).unwrap_or_default());
    }
    result.expect("e2e 超时");
}

/// 会话内消息 id 按 sort_ts 的顺序（只取文字消息）。
fn text_order(t: &TC, conv: &str) -> Vec<String> {
    t.msgs(conv).into_iter().filter(|m| matches!(m.body, MessageBody::Text { .. })).map(|m| m.id).collect()
}

/// v3 收尾（K1–K6）：真实服务端上的邮箱缓存、双方顺序一致、转发附件、@提及计数、草稿、删除会话。
async fn flow_v3(url: &str) {
    let ann = TC::register(url, "Ann").await;
    let ben = TC::register(url, "Ben").await;
    let cat = TC::register(url, "Cat").await;
    for t in [&ann, &ben, &cat] {
        eventually("WS 已连接", || (t.c.connection_state() == ConnectionState::Connected).then_some(())).await;
    }
    make_friends(&ann, &ben).await;
    make_friends(&ann, &cat).await;

    // ---------- K2：邮箱搜索不清掉缓存的好友邮箱 ----------
    ann.c.refresh_friends().await.unwrap();
    assert_eq!(ann.c.cached_profile(ben.uid.clone()).unwrap().email.as_deref(), Some("ben@hush.test"));
    let found = ann.c.search_user_by_email("ben@hush.test".into()).await.unwrap().unwrap();
    assert_eq!(found.email.as_deref(), Some("ben@hush.test"), "搜索结果合并缓存的邮箱");
    assert_eq!(ann.c.cached_profile(ben.uid.clone()).unwrap().email.as_deref(), Some("ben@hush.test"));
    eprintln!("✓ K2 邮箱缓存");

    // ---------- K3：发出的消息 sort_ts = server_ts，双方顺序一致 ----------
    let ab = ann.c.direct_conversation_id(ben.uid.clone());
    let ba = ben.c.direct_conversation_id(ann.uid.clone());
    let mut sent = Vec::new();
    for i in 0..6 {
        let (who, conv) = if i % 2 == 0 { (&ann, &ab) } else { (&ben, &ba) };
        let m = who.c.send_text(conv.clone(), format!("顺序 {i}"), None).await.unwrap();
        assert_eq!(m.status, MessageStatus::Sent);
        assert!(m.server_ts > 0 && m.sort_ts >= m.server_ts && m.sort_ts - m.server_ts < 5, "发出的消息以服务端时间排序: {m:?}");
        eventually("发出消息的 MessageUpdated（sort_ts = 服务端时间）", || {
            who.has_event(|e| matches!(e, CoreEvent::MessageUpdated { message, .. } if message.id == m.id && message.sort_ts == m.sort_ts)).then_some(())
        })
        .await;
        sent.push(m.id);
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    eventually("双方都收到 6 条", || (text_order(&ann, &ab).len() >= 6 && text_order(&ben, &ba).len() >= 6).then_some(())).await;
    assert_eq!(text_order(&ann, &ab), sent, "Ann 一侧按服务端时间排序");
    assert_eq!(text_order(&ben, &ba), sent, "Ben 一侧顺序与 Ann 一致");
    for (a, b) in ann.msgs(&ab).iter().zip(ben.msgs(&ba).iter()) {
        assert_eq!((a.id.as_str(), a.server_ts), (b.id.as_str(), b.server_ts));
    }

    // 群：三人交替发送，三方顺序一致（sort_ts 取 /v1/groups/{gid}/messages 返回的 server_ts）
    let g = ann.c.create_group("v3 群".into(), vec![ben.uid.clone(), cat.uid.clone()]).await.unwrap();
    let gc = format!("g:{}", g.id);
    for t in [&ben, &cat] {
        eventually("成员收到建群", || t.c.group_info(g.id.clone()).unwrap().filter(|x| x.active)).await;
    }
    let mut gsent = Vec::new();
    for i in 0..6 {
        let who = [&ann, &ben, &cat][i % 3];
        let m = who.c.send_text(gc.clone(), format!("群顺序 {i}"), None).await.unwrap();
        assert!(m.server_ts > 0 && m.sort_ts >= m.server_ts && m.sort_ts - m.server_ts < 5);
        gsent.push(m.id);
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    for t in [&ann, &ben, &cat] {
        eventually("三方都收到 6 条群消息", || (text_order(t, &gc).len() >= 6).then_some(())).await;
        assert_eq!(text_order(t, &gc), gsent, "{} 的群消息顺序", t.name);
    }
    eprintln!("✓ K3 双方 / 三方顺序一致");

    // ---------- K6：@提及 ----------
    let ann_name = "@Ann";
    let text = format!("😀{ann_name} 看这里");
    let mentions = vec![
        Mention { user_id: ann.uid.clone(), start: 2, length: 4 },
        Mention { user_id: cat.uid.clone(), start: 90, length: 3 }, // 越界：丢弃
    ];
    let bm = ben.c.send_text_with_mentions(gc.clone(), text.clone(), None, mentions).await.unwrap();
    assert_eq!(bm.mentions, vec![Mention { user_id: ann.uid.clone(), start: 2, length: 4 }]);
    let got = eventually("Ann 收到提及", || ann.msgs(&gc).into_iter().find(|m| m.id == bm.id)).await;
    assert_eq!(got.mentions, bm.mentions);
    eventually("Ann 的 MessageAdded.mentions_me = true", || {
        ann.has_event(|e| matches!(e, CoreEvent::MessageAdded { message, mentions_me: true, .. } if message.id == bm.id)).then_some(())
    })
    .await;
    eventually("Cat 的 MessageAdded.mentions_me = false", || {
        cat.has_event(|e| matches!(e, CoreEvent::MessageAdded { message, mentions_me: false, .. } if message.id == bm.id)).then_some(())
    })
    .await;
    let s = ann.c.conversation(gc.clone()).unwrap().unwrap();
    assert_eq!(s.unread_mentions, 1);
    assert_eq!(cat.c.conversation(gc.clone()).unwrap().unwrap().unread_mentions, 0);
    ann.c.mark_read(gc.clone()).await.unwrap();
    assert_eq!(ann.c.conversation(gc.clone()).unwrap().unwrap().unread_mentions, 0);
    eprintln!("✓ K6 @提及计数");

    // ---------- K4：转发附件（复用密文）；部分目标失败 ----------
    let payload: Vec<u8> = (0..150_000u32).map(|i| (i.wrapping_mul(17) % 251) as u8).collect();
    let fpath = ann._dir.path().join("fwd.bin");
    std::fs::write(&fpath, &payload).unwrap();
    let meta = AttachmentMeta { file_name: Some("转发.bin".into()), ..Default::default() };
    let am = ann
        .c
        .send_attachment(ab.clone(), fpath.to_string_lossy().into(), "application/octet-stream".into(), AttachmentKind::File, Some("原件".into()), meta)
        .await
        .unwrap();
    let orig_id = match &am.body {
        MessageBody::Attachments { items, .. } => items[0].id.clone(),
        other => panic!("{other:?}"),
    };
    // Ben 收到后（不下载）转发给 Cat（同群，可经 Olm 通信）、群，以及一个不存在的群
    let bmsg = eventually("Ben 收到附件", || ben.msgs(&ba).into_iter().find(|m| m.id == am.id)).await;
    let cb = ben.c.direct_conversation_id(cat.uid.clone());
    let out = ben.c.forward_message(bmsg.id.clone(), vec![cb.clone(), gc.clone(), "g:no-such-group".into()]).await.unwrap();
    assert_eq!(out.len(), 3);
    assert_eq!((out[0].status, out[1].status, out[2].status), (MessageStatus::Sent, MessageStatus::Sent, MessageStatus::Failed));
    assert!(out.iter().all(|m| m.forwarded));
    for m in &out {
        match &m.body {
            MessageBody::Attachments { items, caption } => {
                assert_eq!(items[0].id, orig_id, "复用原密文 id，不重新上传");
                assert_eq!(caption.as_deref(), Some("原件"));
            }
            other => panic!("{other:?}"),
        }
    }
    let bc = cat.c.direct_conversation_id(ben.uid.clone());
    for conv in [&bc, &gc] {
        let cm = eventually("Cat 收到转发", || cat.msgs(conv).into_iter().find(|m| m.forwarded && !m.is_outgoing)).await;
        let p = cat.c.download_attachment(cm.id.clone(), 0).await.unwrap();
        assert_eq!(std::fs::read(&p).unwrap(), payload, "接收方可下载并解密转发的附件");
    }
    // 文字转发
    let tf = ben.c.forward_message(bm.id.clone(), vec![cb.clone()]).await.unwrap();
    assert!(tf[0].forwarded && tf[0].mentions.is_empty(), "提及不转发");
    eventually("Cat 收到转发的文字", || cat.msgs(&bc).into_iter().find(|m| m.id == tf[0].id && m.forwarded)).await;
    eprintln!("✓ K4 转发");

    // ---------- K5：草稿 ----------
    ann.c.set_draft(ab.clone(), Some("还没写完".into())).unwrap();
    let s = ann.c.conversation(ab.clone()).unwrap().unwrap();
    assert_eq!(s.draft.as_deref(), Some("还没写完"));
    assert_eq!(ann.c.conversations(false).unwrap()[0].id, ab, "有草稿的会话按草稿时间排在前面");
    ann.c.send_text(ab.clone(), "写完了".into(), None).await.unwrap();
    assert!(ann.c.conversation(ab.clone()).unwrap().unwrap().draft.is_none(), "发送后清空草稿");
    eprintln!("✓ K5 草稿");

    // ---------- K1：删除群会话，群有新消息时重新出现 ----------
    // 先等 Ann 收到转发到群里的附件，免得它在删除之后才到、让会话带着旧消息重新出现
    eventually("Ann 收到转发到群的附件", || ann.msgs(&gc).into_iter().find(|m| m.id == out[1].id)).await;
    ann.c.delete_conversation(gc.clone()).unwrap();
    assert!(ann.c.conversation(gc.clone()).unwrap().is_none());
    assert!(ann.c.messages(gc.clone(), None, 50).unwrap().is_empty());
    assert!(ann.c.group_info(g.id.clone()).unwrap().unwrap().active, "删除会话不退群");
    let nm = cat.c.send_text(gc.clone(), "删除之后的新消息".into(), None).await.unwrap();
    let back = eventually("会话重新出现", || ann.c.conversation(gc.clone()).unwrap()).await;
    assert_eq!(back.title, "v3 群");
    let msgs = ann.c.messages(gc.clone(), None, 50).unwrap();
    assert_eq!(msgs.iter().map(|m| m.id.clone()).collect::<Vec<_>>(), vec![nm.id], "只有新消息");
    eprintln!("✓ K1 删除会话");
}

async fn eventually_async_envs(t: &TC) -> Vec<hushcore::api::Envelope> {
    let deadline = Instant::now() + TIMEOUT;
    loop {
        let v = t.c.debug_fetch_envelopes().await;
        if !v.is_empty() {
            return v;
        }
        assert!(Instant::now() < deadline, "等待信封入队超时");
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

async fn futures_join_all<T: Send + 'static>(hs: impl Iterator<Item = tokio::task::JoinHandle<T>>) -> Vec<T> {
    let mut out = Vec::new();
    for h in hs.collect::<Vec<_>>() {
        out.push(h.await.unwrap());
    }
    out
}

/// 读取直到连接关闭，返回关闭码。
async fn wait_close_code(mut ws: hushcore::ws::WsStream) -> Option<u16> {
    use futures_util::StreamExt;
    let fut = async {
        while let Some(msg) = ws.next().await {
            match msg {
                Ok(tokio_tungstenite::tungstenite::Message::Close(Some(f))) => return Some(u16::from(f.code)),
                Ok(tokio_tungstenite::tungstenite::Message::Close(None)) | Err(_) => return None,
                _ => {}
            }
        }
        None
    };
    tokio::time::timeout(TIMEOUT, fut).await.ok().flatten()
}
