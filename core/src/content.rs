//! 加密前的明文内容格式 `Content`（设计文档 4.2）。

use serde::{Deserialize, Serialize};

use crate::types::{AttachmentKind, CallAction, IceCandidate, Mention};

/// 宽松解析会随版本扩展的枚举（A1③）：认识的值照常解析，不认识的（未来版本新增）返回 None，
/// 由各字段决定怎么兜底，而不是让整条内容解析失败。
fn de_lenient<'de, D, T>(d: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned,
{
    let v = serde_json::Value::deserialize(d)?;
    Ok(serde_json::from_value(v).ok())
}

/// 未知的附件类型（例如未来的 `sticker`）按普通文件处理：能显示、能下载。
/// `AttachmentKind` 是导出给两端的枚举，不在上面加变体（两端的 `when` / `switch` 是穷尽的）。
fn de_attachment_kind<'de, D: serde::Deserializer<'de>>(d: D) -> Result<AttachmentKind, D::Error> {
    Ok(de_lenient(d)?.unwrap_or(AttachmentKind::File))
}

/// 未知的通话信令动作解析为 None，接收方忽略该信令（`CallAction` 同样是导出枚举，不加变体）。
fn de_call_action<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<CallAction>, D::Error> {
    de_lenient(d)
}

pub const CONTENT_VERSION: u32 = 1;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Content {
    pub v: u32,
    pub id: String,
    pub ts: i64,
    #[serde(default)]
    pub group_id: Option<String>,
    #[serde(default)]
    pub expire_secs: u32,
    /// 扩展字段（设计文档之外，见 CORE_NOTES）：发送方该会话阅后即焚设置最后一次变更的时间（毫秒）。
    /// 接收方只采用比本地更新的设置（后写者胜），0 表示未知（旧版本或从未设置），缺省时不序列化。
    #[serde(default, skip_serializing_if = "is_zero")]
    pub expire_set_at: i64,
    /// 扩展字段（v3 / K4）：转发的消息。为 false 时不序列化；旧版本忽略未知字段（本结构没有 deny_unknown_fields）。
    #[serde(default, skip_serializing_if = "is_false")]
    pub forwarded: bool,
    pub body: ContentBody,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

fn is_false(v: &bool) -> bool {
    !*v
}

/// 校验 @提及区间（v3 / K6）：丢弃 user_id 为空、长度为 0、越界（`start + length` 超过文本 UTF-16 长度）
/// 以及起止落在代理对中间的项，不报错；同一区间重复的只保留一个。结果按 `start` 排序。
pub fn valid_mentions(text: &str, mentions: Vec<Mention>) -> Vec<Mention> {
    let units: Vec<u16> = text.encode_utf16().collect();
    let len = units.len() as u64;
    // 位置 i 处于代理对中间（低代理项）时不能作为边界
    let splits_pair = |i: u64| i > 0 && i < len && (0xDC00..=0xDFFF).contains(&units[i as usize]);
    let mut out: Vec<Mention> = mentions
        .into_iter()
        .filter(|m| {
            let (s, l) = (m.start as u64, m.length as u64);
            !m.user_id.is_empty() && l > 0 && s + l <= len && !splits_pair(s) && !splits_pair(s + l)
        })
        .collect();
    out.sort_by_key(|m| (m.start, m.length));
    out.dedup_by(|a, b| a.start == b.start && a.length == b.length);
    out
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct WireQuote {
    pub id: String,
    pub author: String,
    #[serde(default)]
    pub text: String,
}

impl From<&crate::types::Quote> for WireQuote {
    fn from(q: &crate::types::Quote) -> Self {
        WireQuote { id: q.id.clone(), author: q.author.clone(), text: q.text.clone() }
    }
}

impl From<WireQuote> for crate::types::Quote {
    fn from(q: WireQuote) -> Self {
        crate::types::Quote { id: q.id, author: q.author, text: q.text }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct WireAttachment {
    pub id: String,
    /// 32 字节 AES-256-GCM 密钥（base64）
    pub key: String,
    /// 密文 SHA-256（base64）
    pub digest: String,
    pub size: u64,
    #[serde(default)]
    pub mime: String,
    #[serde(default)]
    pub file_name: String,
    #[serde(deserialize_with = "de_attachment_kind")]
    pub kind: AttachmentKind,
    #[serde(default)]
    pub width: u32,
    #[serde(default)]
    pub height: u32,
    #[serde(default)]
    pub duration_ms: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thumbnail: Option<String>,
    /// 密文格式（C-I）：1 = 整文件 GCM（旧版本，缺省），2 = 分块流式 GCM。见 `crypto::attachment`。
    #[serde(default = "format_v1")]
    pub format: u32,
}

fn format_v1() -> u32 {
    crate::crypto::attachment::FORMAT_V1
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReceiptKind {
    Delivered,
    Read,
    /// 未来版本新增的回执类型：忽略。
    #[serde(other)]
    Unknown,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TypingAction {
    Started,
    Stopped,
    /// 未来版本新增的输入状态：忽略。
    #[serde(other)]
    Unknown,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ContentBody {
    Text {
        text: String,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        quote: Option<WireQuote>,
        /// @提及（v3 / K6）：UTF-16 偏移。为空时不序列化，旧版本忽略。
        #[serde(default, skip_serializing_if = "Vec::is_empty")]
        mentions: Vec<Mention>,
    },
    Attachment {
        attachments: Vec<WireAttachment>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        caption: Option<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        quote: Option<WireQuote>,
    },
    Reaction {
        target_id: String,
        target_author: String,
        emoji: String,
        #[serde(default)]
        remove: bool,
    },
    Delete {
        target_id: String,
    },
    Receipt {
        kind: ReceiptKind,
        ids: Vec<String>,
    },
    Typing {
        action: TypingAction,
    },
    Timer {
        expire_secs: u32,
    },
    Call {
        call_id: String,
        /// 本版本不认识的动作为 None（见 `de_call_action`）
        #[serde(deserialize_with = "de_call_action")]
        action: Option<CallAction>,
        #[serde(default)]
        video: bool,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        sdp: Option<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        candidates: Option<Vec<IceCandidate>>,
    },
    RoomKey {
        group_id: String,
        session_id: String,
        session_key: String,
    },
    SessionReset {},
    ProfileKey {},
    GroupUpdate {},
    /// 未来版本新增的类型：忽略。
    #[serde(other)]
    Unknown,
}

impl Content {
    pub fn new(body: ContentBody, group_id: Option<String>, expire_secs: u32) -> Self {
        Content {
            v: CONTENT_VERSION,
            id: crate::util::new_id(),
            ts: crate::util::now_ms(),
            group_id,
            expire_secs,
            expire_set_at: 0,
            forwarded: false,
            body,
        }
    }

    pub fn to_bytes(&self) -> Vec<u8> {
        serde_json::to_vec(self).expect("Content 序列化不会失败")
    }

    #[cfg(test)]
    pub fn from_bytes(b: &[u8]) -> crate::error::Result<Self> {
        Ok(serde_json::from_slice(b)?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn text_matches_spec_example() {
        let raw = json!({"v":1,"id":"m1","ts":1727000000000i64,"group_id":null,"expire_secs":0,
            "body":{"type":"text","text":"你好","quote":{"id":"q","author":"u","text":"t"}}});
        let c: Content = serde_json::from_value(raw.clone()).unwrap();
        assert_eq!(c.id, "m1");
        match &c.body {
            ContentBody::Text { text, quote, .. } => {
                assert_eq!(text, "你好");
                assert_eq!(quote.as_ref().unwrap().author, "u");
            }
            _ => panic!("wrong body"),
        }
        assert_eq!(serde_json::to_value(&c).unwrap(), raw);
    }

    #[test]
    fn all_body_types_roundtrip() {
        let bodies = vec![
            ContentBody::Text { text: "hi".into(), quote: None, mentions: vec![] },
            ContentBody::Text {
                text: "@小明 你好".into(),
                quote: None,
                mentions: vec![Mention { user_id: "u1".into(), start: 0, length: 3 }],
            },
            ContentBody::Attachment {
                attachments: vec![WireAttachment {
                    id: "a".into(),
                    key: "k".into(),
                    digest: "d".into(),
                    size: 3,
                    mime: "image/png".into(),
                    file_name: "x.png".into(),
                    kind: AttachmentKind::Image,
                    width: 1,
                    height: 2,
                    duration_ms: 0,
                    thumbnail: Some("dGh1bWI=".into()),
                    format: 2,
                }],
                caption: Some("cap".into()),
                quote: None,
            },
            ContentBody::Reaction { target_id: "t".into(), target_author: "u".into(), emoji: "👍".into(), remove: false },
            ContentBody::Delete { target_id: "t".into() },
            ContentBody::Receipt { kind: ReceiptKind::Read, ids: vec!["a".into(), "b".into()] },
            ContentBody::Typing { action: TypingAction::Started },
            ContentBody::Timer { expire_secs: 30 },
            ContentBody::Call {
                call_id: "c".into(),
                action: Some(CallAction::Offer),
                video: true,
                sdp: Some("v=0".into()),
                candidates: Some(vec![IceCandidate { sdp: "cand".into(), sdp_mid: Some("0".into()), sdp_mline_index: Some(0) }]),
            },
            ContentBody::RoomKey { group_id: "g".into(), session_id: "s".into(), session_key: "k".into() },
            ContentBody::SessionReset {},
            ContentBody::ProfileKey {},
            ContentBody::GroupUpdate {},
        ];
        for b in bodies {
            let c = Content::new(b, Some("g".into()), 5);
            let back = Content::from_bytes(&c.to_bytes()).unwrap();
            assert_eq!(c, back);
        }
    }

    #[test]
    fn wire_names() {
        let c = Content::new(ContentBody::Receipt { kind: ReceiptKind::Delivered, ids: vec!["x".into()] }, None, 0);
        let v = serde_json::to_value(&c).unwrap();
        assert_eq!(v["body"]["type"], "receipt");
        assert_eq!(v["body"]["kind"], "delivered");
        let c = Content::new(ContentBody::SessionReset {}, None, 0);
        assert_eq!(serde_json::to_value(&c).unwrap()["body"], json!({"type":"session_reset"}));
        let c = Content::new(
            ContentBody::RoomKey { group_id: "g".into(), session_id: "s".into(), session_key: "k".into() },
            None,
            0,
        );
        assert_eq!(serde_json::to_value(&c).unwrap()["body"]["type"], "room_key");
        let c = Content::new(ContentBody::Typing { action: TypingAction::Stopped }, None, 0);
        assert_eq!(serde_json::to_value(&c).unwrap()["body"]["action"], "stopped");
    }

    /// 旧版本发出的附件没有 format 字段：按 v1 处理。
    #[test]
    fn attachment_format_defaults_to_v1() {
        let raw = json!({"v":1,"id":"m","ts":1,"body":{"type":"attachment","attachments":[
            {"id":"a","key":"k","digest":"d","size":3,"kind":"file"}]}});
        let c: Content = serde_json::from_value(raw).unwrap();
        match c.body {
            ContentBody::Attachment { attachments, .. } => assert_eq!(attachments[0].format, 1),
            _ => panic!("应为附件"),
        }
    }

    /// v3：旧版本发来的内容没有 forwarded / mentions；新版本发出的内容带着它们、以及未来的未知字段时，
    /// 按当前结构解析也不能失败（Content / ContentBody 都没有 deny_unknown_fields）。
    #[test]
    fn optional_v3_fields_and_unknown_fields_tolerated() {
        let old = json!({"v":1,"id":"m","ts":1,"body":{"type":"text","text":"x"}});
        let c: Content = serde_json::from_value(old.clone()).unwrap();
        assert!(!c.forwarded);
        assert!(matches!(&c.body, ContentBody::Text { mentions, .. } if mentions.is_empty()));
        // 缺省值不序列化：与旧版本发出的一致
        assert_eq!(serde_json::to_value(&c).unwrap()["body"], old["body"]);
        assert!(serde_json::to_value(&c).unwrap().get("forwarded").is_none());

        let new = json!({"v":1,"id":"m","ts":1,"forwarded":true,"zzz":{"future":1},
            "body":{"type":"text","text":"@a 好","future":[1,2],
                "mentions":[{"user_id":"a","start":0,"length":2,"color":"red"}]}});
        let c: Content = serde_json::from_value(new).unwrap();
        assert!(c.forwarded);
        match &c.body {
            ContentBody::Text { mentions, .. } => assert_eq!(mentions, &vec![Mention { user_id: "a".into(), start: 0, length: 2 }]),
            other => panic!("{other:?}"),
        }
        let att = json!({"v":1,"id":"m","ts":1,"forwarded":true,"body":{"type":"attachment","extra":true,
            "attachments":[{"id":"a","key":"k","digest":"d","size":3,"kind":"file","format":2,"new_field":"x"}]}});
        let c: Content = serde_json::from_value(att).unwrap();
        assert!(c.forwarded && matches!(c.body, ContentBody::Attachment { .. }));
    }

    /// @提及区间校验：UTF-16 偏移（emoji 占 2 个码元），越界 / 空 / 切开代理对的丢弃。
    #[test]
    fn mention_ranges_validated_in_utf16() {
        let text = "😀@小明 hi"; // 😀 = 2 个 UTF-16 码元，共 2+3+3 = 8
        assert_eq!(text.encode_utf16().count(), 8);
        let m = |u: &str, s: u32, l: u32| Mention { user_id: u.into(), start: s, length: l };
        let got = valid_mentions(
            text,
            vec![m("b", 2, 3), m("", 2, 3), m("c", 2, 0), m("d", 6, 3), m("e", 1, 2), m("f", 0, 1), m("g", 7, 1), m("b", 2, 3), m("h", u32::MAX, 2)],
        );
        assert_eq!(got, vec![m("b", 2, 3), m("g", 7, 1)]);
    }

    /// A1③：会随版本扩展的枚举遇到未知值时兜底，整条内容照常解析。
    #[test]
    fn unknown_enum_values_tolerated() {
        let parse = |body: serde_json::Value| serde_json::from_value::<Content>(json!({"v":1,"id":"m","ts":1,"body":body})).unwrap().body;
        match parse(json!({"type":"attachment","attachments":[{"id":"a","key":"k","digest":"d","size":3,"kind":"sticker"}]})) {
            ContentBody::Attachment { attachments, .. } => assert_eq!(attachments[0].kind, AttachmentKind::File),
            other => panic!("{other:?}"),
        }
        assert_eq!(parse(json!({"type":"receipt","kind":"played","ids":["x"]})), ContentBody::Receipt { kind: ReceiptKind::Unknown, ids: vec!["x".into()] });
        assert_eq!(parse(json!({"type":"typing","action":"recording"})), ContentBody::Typing { action: TypingAction::Unknown });
        assert!(matches!(parse(json!({"type":"call","call_id":"c","action":"hold"})), ContentBody::Call { action: None, .. }));
        assert!(matches!(parse(json!({"type":"call","call_id":"c","action":"offer"})), ContentBody::Call { action: Some(CallAction::Offer), .. }));
        // 已知值的线上格式不变
        let c = Content::new(ContentBody::Call { call_id: "c".into(), action: Some(CallAction::Hangup), video: false, sdp: None, candidates: None }, None, 0);
        assert_eq!(serde_json::to_value(&c).unwrap()["body"], json!({"type":"call","call_id":"c","action":"hangup","video":false}));
    }

    #[test]
    fn unknown_type_tolerated() {
        let raw = json!({"v":1,"id":"m","ts":1,"body":{"type":"sticker","pack":"x"}});
        let c: Content = serde_json::from_value(raw).unwrap();
        assert_eq!(c.body, ContentBody::Unknown);
        assert_eq!(c.group_id, None);
        assert_eq!(c.expire_secs, 0);
    }
}
