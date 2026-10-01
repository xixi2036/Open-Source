//! iOS UI 回归测试用的机器人（HushUITests/A14AuditFixTests 依赖）：在 core/examples/bot.rs 的基础上增加几条指令，
//! 让「来电在若干秒后到达」「对方连发 N 条」这类场景可以由测试自己通过聊天消息触发，不需要宿主机脚本配合。
//!
//! 额外指令（单聊里发送）：
//! - `延迟来电 N` / `延迟视频 N`：N 秒后发起语音 / 视频来电（只有信令）
//! - `响铃取消 N M`：N 秒后来电，再过 M 秒主叫取消
//! - `连发 N`：连续发送 N 条文字「连发 i/N」
//! - `延迟连发 D N`：D 秒后连续发送 N 条文字「延迟连发 i/N」
//! - `链接`：回复一条带 https 与 http 链接的消息
//!
//! 用法见 ios/scripts/run-ui-host.sh（把本文件复制到 core/examples/ 后 `cargo build --example auditbot`）。
//!
//! ---- 以下为 bot.rs 原有说明 ----
//! Hush 测试机器人：一个真实的加密客户端，用于在模拟器上联调 App。
//!
//! 行为：
//! - 自动同意好友申请；可用 `--add <email>` 主动加好友
//! - 收到文字回复「收到：…」，并给消息点 👍；收到附件回复附件数量
//! - 指令：`图片` 回一张图片；`语音` 回一段音频文件（需 --voice）；`打电话`/`视频` 发起来电（只有信令，无媒体）；
//!   `阅后即焚` 把会话设为 30 秒；`撤回` 发一条消息 3 秒后撤回；`群` 与所有好友建群并在群里打招呼
//! - 群消息里被 @小助手 或发送 `机器人` 时在群里回复；被 @提及（`mentions_me`）时回复「@对方 收到@」（带提及，便于联调）
//! - 启动时发一条朋友圈；有人评论其朋友圈时回复评论
//!
//! 运行：`cargo run --example bot -- --server http://127.0.0.1:8080 --email bot@hush.local --name 小助手`

use std::sync::Arc;
use std::time::Duration;

use hushcore::*;
use tokio::sync::mpsc;

struct Listener(mpsc::UnboundedSender<CoreEvent>);

impl EventListener for Listener {
    fn on_event(&self, event: CoreEvent) {
        let _ = self.0.send(event);
    }
}

fn arg(name: &str, default: &str) -> String {
    let args: Vec<String> = std::env::args().collect();
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1).cloned())
        .unwrap_or_else(|| default.to_string())
}

#[tokio::main]
async fn main() -> std::result::Result<(), Box<dyn std::error::Error>> {
    let server = arg("--server", "http://127.0.0.1:8080");
    let email = arg("--email", "bot@hush.local");
    let name = arg("--name", "小助手");
    let data = arg("--data", &format!("/tmp/hushbot-{}", email.replace('@', "_")));
    let image = arg("--image", concat!(env!("CARGO_MANIFEST_DIR"), "/../assets/app_icon_1024.png"));
    let voice = arg("--voice", "");
    let add = arg("--add", "");

    std::fs::create_dir_all(&data)?;
    let client = HushClient::new(data.clone(), vec![7u8; 32], "bot".into(), "hush-bot".into())?;
    let (tx, mut rx) = mpsc::unbounded_channel();
    client.set_listener(Arc::new(Listener(tx)));

    if !client.is_logged_in() {
        client.request_code(server.clone(), email.clone()).await.ok();
        let acc = client.verify_code(server.clone(), email.clone(), "123456".into()).await?;
        println!("[bot] 登录成功 user_id={}", acc.user_id);
        client.update_profile(Some(name.clone()), Some("我是 Hush 测试机器人，发「帮助」看指令".into()), Some(image.clone())).await?;
        client
            .post_moment(format!("大家好，我是{name} 👋 这是一条测试朋友圈"), vec![image.clone()], Visibility { mode: VisibilityMode::All, user_ids: vec![] })
            .await?;
    }
    let me = client.my_account().expect("已登录");
    println!("[bot] {} ({}) 就绪，user_id={}", me.profile.name, me.email, me.user_id);
    client.start()?;

    if !add.is_empty() {
        match client.search_user_by_email(add.clone()).await? {
            Some(p) => {
                client.send_friend_request(p.id.clone(), format!("我是{name}")).await?;
                println!("[bot] 已向 {} 发送好友申请", p.name);
            }
            None => println!("[bot] 找不到用户 {add}"),
        }
    }

    // 定时处理待同意的好友申请（兜底，防止事件在离线期间被清空）
    let c2 = client.clone();
    tokio::spawn(async move {
        loop {
            if let Ok(reqs) = c2.list_friend_requests().await {
                for r in reqs.incoming.into_iter().filter(|r| r.status == "pending") {
                    if c2.accept_friend_request(r.id.clone()).await.is_ok() {
                        println!("[bot] 同意了 {} 的好友申请", r.from.name);
                    }
                }
            }
            tokio::time::sleep(Duration::from_secs(5)).await;
        }
    });

    while let Some(ev) = rx.recv().await {
        let client = client.clone();
        let (image, voice, my_id, name) = (image.clone(), voice.clone(), me.user_id.clone(), name.clone());
        tokio::spawn(async move {
            if let Err(e) = handle(&client, ev, &image, &voice, &my_id, &name).await {
                eprintln!("[bot] 处理事件出错：{e}");
            }
        });
    }
    Ok(())
}

async fn handle(c: &HushClient, ev: CoreEvent, image: &str, voice: &str, my_id: &str, name: &str) -> Result<()> {
    match ev {
        CoreEvent::ConnectionChanged { state } => println!("[bot] 连接状态：{state:?}"),
        CoreEvent::FriendRequestReceived { request } => {
            c.accept_friend_request(request.id).await?;
            println!("[bot] 同意了 {} 的好友申请", request.from.name);
            if request.from.about.contains("测试机器人") {
                return Ok(());
            }
            let conv = c.direct_conversation_id(request.from.id);
            tokio::time::sleep(Duration::from_millis(500)).await;
            c.send_text(conv, format!("你好，我是{name}！发「帮助」看看我能做什么 😊"), None).await?;
        }
        CoreEvent::MessageAdded { conv_id, message, mentions_me, .. } if !message.is_outgoing => {
            let group = conv_id.starts_with("g:");
            c.mark_read(conv_id.clone()).await.ok();
            // 群里被 @：回复「@对方 收到@」，并提及对方（两端联调 @提及的收发与未读计数）
            if group && mentions_me {
                let who = c.cached_profile(message.sender_id.clone()).map(|p| p.name).filter(|n| !n.is_empty()).unwrap_or_else(|| "你".into());
                let tag = format!("@{who}");
                let length = tag.encode_utf16().count() as u32;
                let mention = Mention { user_id: message.sender_id.clone(), start: 0, length };
                c.send_text_with_mentions(conv_id, format!("{tag} 收到@"), Some(message.id), vec![mention]).await?;
                return Ok(());
            }
            let text = match &message.body {
                MessageBody::Text { text } => text.clone(),
                MessageBody::Attachments { items, .. } => {
                    if !group {
                        c.send_text(conv_id, format!("收到 {} 个附件 📎", items.len()), Some(message.id)).await?;
                    }
                    return Ok(());
                }
                _ => return Ok(()),
            };
            println!("[bot] 收到 {conv_id}：{text}");
            // 不回复其他机器人（避免两个机器人互相复读）
            if c.cached_profile(message.sender_id.clone()).map(|p| p.about.contains("测试机器人")).unwrap_or(false) {
                return Ok(());
            }
            if group && !(text.contains(name) || text.contains("机器人")) {
                return Ok(());
            }
            let typing_conv = conv_id.clone();
            c.set_typing(conv_id.clone(), true).await.ok();
            tokio::time::sleep(Duration::from_millis(800)).await;
            c.react(message.id.clone(), "👍".into(), false).await.ok();
            let peer = message.sender_id.clone();
            match text.trim() {
                "帮助" => {
                    c.send_text(conv_id, "指令：图片 / 语音 / 打电话 / 视频 / 阅后即焚 / 撤回 / 群。其他文字我会复述。".into(), None).await?;
                }
                "图片" => {
                    let meta = AttachmentMeta { file_name: Some("hush.png".into()), width: 1024, height: 1024, duration_ms: 0, thumbnail: None };
                    c.send_attachment(conv_id, image.into(), "image/png".into(), AttachmentKind::Image, Some("给你一张图".into()), meta).await?;
                }
                "语音" if !voice.is_empty() => {
                    let meta = AttachmentMeta { file_name: Some("voice.m4a".into()), width: 0, height: 0, duration_ms: 3000, thumbnail: None };
                    c.send_attachment(conv_id, voice.into(), "audio/mp4".into(), AttachmentKind::Voice, None, meta).await?;
                }
                "打电话" | "视频" => {
                    let signal = CallSignal {
                        call_id: uuid_like(),
                        action: CallAction::Offer,
                        video: text.trim() == "视频",
                        sdp: Some("v=0\r\n".into()),
                        candidates: vec![],
                    };
                    c.send_call_signal(peer, signal).await?;
                }
                "阅后即焚" => {
                    c.set_disappearing(conv_id.clone(), 30).await?;
                    c.send_text(conv_id.clone(), "这条消息 30 秒后消失 🔥".into(), None).await?;
                }
                "撤回" => {
                    let m = c.send_text(conv_id, "这条消息马上会被撤回".into(), None).await?;
                    tokio::time::sleep(Duration::from_secs(3)).await;
                    c.delete_for_everyone(m.id).await?;
                }
                "群" => {
                    let ids: Vec<String> = c.friends()?.into_iter().map(|p| p.id).filter(|id| id != my_id).collect();
                    let g = c.create_group("机器人测试群".into(), ids).await?;
                    c.send_text(format!("g:{}", g.id), "欢迎来到测试群！".into(), None).await?;
                }
                cmd if cmd.starts_with("延迟来电") || cmd.starts_with("延迟视频") || cmd.starts_with("响铃取消") => {
                    let nums: Vec<u64> = cmd.split_whitespace().skip(1).filter_map(|s| s.parse().ok()).collect();
                    let delay = nums.first().copied().unwrap_or(5);
                    let cancel_after = if cmd.starts_with("响铃取消") { Some(nums.get(1).copied().unwrap_or(5)) } else { None };
                    let video = cmd.starts_with("延迟视频");
                    c.send_text(conv_id, format!("{delay} 秒后呼叫你"), None).await?;
                    tokio::time::sleep(Duration::from_secs(delay)).await;
                    let call_id = uuid_like();
                    let signal = CallSignal { call_id: call_id.clone(), action: CallAction::Offer, video, sdp: Some("v=0\r\n".into()), candidates: vec![] };
                    c.send_call_signal(peer.clone(), signal).await?;
                    if let Some(m) = cancel_after {
                        tokio::time::sleep(Duration::from_secs(m)).await;
                        let bye = CallSignal { call_id, action: CallAction::Hangup, video, sdp: None, candidates: vec![] };
                        c.send_call_signal(peer, bye).await?;
                    }
                }
                cmd if cmd.starts_with("延迟连发") => {
                    let nums: Vec<u64> = cmd.split_whitespace().skip(1).filter_map(|s| s.parse().ok()).collect();
                    let (delay, n) = (nums.first().copied().unwrap_or(5), nums.get(1).copied().unwrap_or(20));
                    tokio::time::sleep(Duration::from_secs(delay)).await;
                    for i in 1..=n {
                        c.send_text(conv_id.clone(), format!("延迟连发 {i}/{n}"), None).await?;
                    }
                }
                cmd if cmd.starts_with("连发") => {
                    let n: u32 = cmd.split_whitespace().nth(1).and_then(|s| s.parse().ok()).unwrap_or(50);
                    for i in 1..=n {
                        c.send_text(conv_id.clone(), format!("连发 {i}/{n}"), None).await?;
                    }
                }
                "链接" => {
                    c.send_text(conv_id, "官网 https://example.com/hush?a=1 ，内网 http://192.168.1.10:8080/x ，结束".into(), None).await?;
                }
                other => {
                    c.send_text(conv_id.clone(), format!("收到：{other}"), Some(message.id)).await?;
                }
            }
            c.set_typing(typing_conv, false).await.ok();
        }
        CoreEvent::CallSignalReceived { from_user_id, signal } => {
            if signal.action == CallAction::Offer {
                let reply = CallSignal { call_id: signal.call_id, action: CallAction::Busy, video: signal.video, sdp: None, candidates: vec![] };
                c.send_call_signal(from_user_id, reply).await?;
            }
        }
        CoreEvent::MomentNotification { notification } if notification.kind == MomentNotificationKind::Comment => {
            c.comment_moment(notification.moment_id, "谢谢评论 🙏".into(), Some(notification.from.id)).await?;
        }
        _ => {}
    }
    Ok(())
}

fn uuid_like() -> String {
    let t = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos();
    format!("{:08x}-0000-4000-8000-{:012x}", (t >> 64) as u32, t as u64 & 0xffff_ffff_ffff)
}
