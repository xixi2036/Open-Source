//! 导出到 Kotlin/Swift 的数据类型（UniFFI Record / Enum）。
//! 部分类型同时实现 serde，用于直接解析服务端 JSON（契约第 3 节）。

use serde::{Deserialize, Serialize};

fn de_opt_string<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<String>, D::Error> {
    let v: Option<String> = Option::deserialize(d)?;
    Ok(v.filter(|s| !s.is_empty()))
}

/// 用户资料 `Profile = {id, name, about, avatar_id, updated_at, email?}`。
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct Profile {
    pub id: String,
    pub name: String,
    pub about: String,
    #[serde(deserialize_with = "de_opt_string")]
    pub avatar_id: Option<String>,
    pub updated_at: i64,
    /// 邮箱（v2 / S-B）：仅本人或互为好友时服务端才返回，否则为 None。
    #[serde(deserialize_with = "de_opt_string", skip_serializing_if = "Option::is_none")]
    pub email: Option<String>,
}

/// 当前登录账号。
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct MyAccount {
    pub user_id: String,
    pub email: String,
    pub server_url: String,
    pub profile: Profile,
    /// 本次 verify 是否为新注册。
    pub is_new: bool,
}

/// 好友申请状态。
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct FriendRequest {
    pub id: String,
    pub from: Profile,
    pub to: Profile,
    pub message: String,
    /// pending / accepted / rejected
    pub status: String,
    pub created_at: i64,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct FriendRequests {
    pub incoming: Vec<FriendRequest>,
    pub outgoing: Vec<FriendRequest>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum ConnectionState {
    Disconnected,
    Connecting,
    Connected,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum ConversationKind {
    Direct,
    Group,
}

/// 会话列表项。
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct ConversationSummary {
    /// `u:<user_id>` 或 `g:<group_id>`
    pub id: String,
    pub kind: ConversationKind,
    pub peer_user_id: Option<String>,
    pub group_id: Option<String>,
    /// 对方昵称或群名
    pub title: String,
    pub avatar_id: Option<String>,
    pub last_message: Option<MessageItem>,
    pub last_activity_ts: i64,
    pub unread_count: u32,
    pub pinned: bool,
    pub muted: bool,
    pub archived: bool,
    /// 当前会话的阅后即焚秒数（0=关闭）
    pub disappearing_secs: u32,
    /// 群聊：我是否仍在群内；单聊恒为 true
    pub active: bool,
    /// 草稿（v3 / K5）：`set_draft` 保存的未发送文本，没有草稿为 None
    pub draft: Option<String>,
    /// 未读消息中提及我的条数（v3 / K6），`mark_read` 清零
    pub unread_mentions: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum MessageStatus {
    Sending,
    Sent,
    Delivered,
    Read,
    Failed,
}

impl MessageStatus {
    pub fn rank(self) -> u8 {
        match self {
            MessageStatus::Failed => 0,
            MessageStatus::Sending => 1,
            MessageStatus::Sent => 2,
            MessageStatus::Delivered => 3,
            MessageStatus::Read => 4,
        }
    }
    pub fn as_str(self) -> &'static str {
        match self {
            MessageStatus::Sending => "sending",
            MessageStatus::Sent => "sent",
            MessageStatus::Delivered => "delivered",
            MessageStatus::Read => "read",
            MessageStatus::Failed => "failed",
        }
    }
    pub fn parse(s: &str) -> Self {
        match s {
            "sending" => MessageStatus::Sending,
            "sent" => MessageStatus::Sent,
            "delivered" => MessageStatus::Delivered,
            "read" => MessageStatus::Read,
            _ => MessageStatus::Failed,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum AttachmentKind {
    Image,
    Voice,
    Video,
    File,
}

/// 消息中的一个附件（不含密钥等敏感信息）。
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct AttachmentItem {
    pub id: String,
    pub mime: String,
    pub file_name: String,
    pub kind: AttachmentKind,
    pub size: u64,
    pub width: u32,
    pub height: u32,
    pub duration_ms: u64,
    /// ≤10KB JPEG 缩略图
    pub thumbnail: Option<Vec<u8>>,
    /// 已下载并解密的本地路径（未下载为 None）
    pub local_path: Option<String>,
}

/// 发送附件时平台层提供的元数据。
#[derive(Debug, Clone, PartialEq, Default, uniffi::Record)]
pub struct AttachmentMeta {
    /// 为空时取 file_path 的文件名
    pub file_name: Option<String>,
    pub width: u32,
    pub height: u32,
    pub duration_ms: u64,
    pub thumbnail: Option<Vec<u8>>,
}

/// 文字消息中的 @提及（v3 / K6）。`start` / `length` 是 UTF-16 码元偏移（与 Android / iOS 文本控件一致）。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, uniffi::Record)]
pub struct Mention {
    pub user_id: String,
    pub start: u32,
    pub length: u32,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, uniffi::Record)]
pub struct Quote {
    pub id: String,
    pub author: String,
    pub text: String,
}

#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct ReactionGroup {
    pub emoji: String,
    pub user_ids: Vec<String>,
}

/// 通话结束原因（v2 / C-B）。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum CallEndReason {
    /// 已接通后结束
    Completed,
    /// 呼入未接
    Missed,
    /// 被拒绝（对方拒接，或本端拒接呼入）
    Rejected,
    /// 对方忙线
    Busy,
    /// 呼出方在接通前取消
    Cancelled,
    /// 通话失败（例如呼叫信令发送失败）
    Failed,
}

#[derive(Debug, Clone, PartialEq, uniffi::Enum)]
pub enum MessageBody {
    Text { text: String },
    Attachments { items: Vec<AttachmentItem>, caption: Option<String> },
    System { text: String },
    Undecryptable,
    CallLog { video: bool, duration_secs: u32, missed: bool, outgoing: bool, reason: CallEndReason },
}

#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct MessageItem {
    pub id: String,
    pub conv_id: String,
    pub sender_id: String,
    pub is_outgoing: bool,
    /// 发送方时间戳（Content.ts）
    pub sent_at: i64,
    /// 服务端时间戳（未知为 0）
    pub server_ts: i64,
    /// 排序时间戳（v2 / C-A），与 `messages(before_ts)` 游标同一口径：翻页时传上一页最早一条的 `sort_ts`。
    pub sort_ts: i64,
    pub status: MessageStatus,
    pub body: MessageBody,
    pub quote: Option<Quote>,
    pub reactions: Vec<ReactionGroup>,
    pub expire_secs: u32,
    pub expires_at: Option<i64>,
    pub deleted: bool,
    /// 转发的消息（v3 / K4）
    pub forwarded: bool,
    /// 文字消息中的 @提及（v3 / K6），已校验区间
    pub mentions: Vec<Mention>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum GroupRole {
    Owner,
    Admin,
    Member,
}

/// 服务端以后新增的群角色按普通成员处理（权限最小），不让整个群信息解析失败。
fn de_group_role<'de, D: serde::Deserializer<'de>>(d: D) -> Result<GroupRole, D::Error> {
    let v = serde_json::Value::deserialize(d)?;
    Ok(serde_json::from_value(v).unwrap_or(GroupRole::Member))
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, uniffi::Record)]
pub struct GroupMember {
    pub user_id: String,
    #[serde(deserialize_with = "de_group_role")]
    pub role: GroupRole,
    #[serde(default)]
    pub joined_at: i64,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct GroupInfo {
    pub id: String,
    pub name: String,
    #[serde(deserialize_with = "de_opt_string")]
    pub avatar_id: Option<String>,
    pub announcement: String,
    pub owner_id: String,
    pub members: Vec<GroupMember>,
    pub updated_at: i64,
    /// 本地字段：我是否仍是成员
    #[serde(skip_deserializing)]
    pub active: bool,
}

impl GroupInfo {
    pub fn member_ids(&self) -> Vec<String> {
        self.members.iter().map(|m| m.user_id.clone()).collect()
    }
    pub fn is_member(&self, uid: &str) -> bool {
        self.members.iter().any(|m| m.user_id == uid)
    }
}

#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct SafetyNumber {
    /// 60 位数字（无分隔，UI 按 12 组×5 位展示）
    pub digits: String,
    /// `hush-sn:v1:<60位>`
    pub qr_payload: String,
    pub verified: bool,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct MomentMedia {
    pub attachment_id: String,
    pub width: u32,
    pub height: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum VisibilityMode {
    #[default]
    All,
    Include,
    Exclude,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct Visibility {
    pub mode: VisibilityMode,
    pub user_ids: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct Comment {
    pub id: String,
    pub author: Profile,
    pub reply_to: Option<Profile>,
    pub text: String,
    pub created_at: i64,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct Moment {
    pub id: String,
    pub author: Profile,
    pub text: String,
    pub media: Vec<MomentMedia>,
    /// 仅作者可见
    pub visibility: Option<Visibility>,
    pub likes: Vec<Profile>,
    pub comments: Vec<Comment>,
    pub created_at: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum MomentNotificationKind {
    #[default]
    Like,
    Comment,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct MomentNotification {
    pub id: String,
    pub kind: MomentNotificationKind,
    pub moment_id: String,
    pub from: Profile,
    pub text: Option<String>,
    pub created_at: i64,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct TurnCredentials {
    pub urls: Vec<String>,
    pub username: String,
    pub credential: String,
    pub ttl: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, uniffi::Enum)]
#[serde(rename_all = "snake_case")]
pub enum CallAction {
    Offer,
    Answer,
    Ice,
    Hangup,
    Busy,
    Reject,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, uniffi::Record)]
pub struct IceCandidate {
    pub sdp: String,
    #[serde(default)]
    pub sdp_mid: Option<String>,
    #[serde(default)]
    pub sdp_mline_index: Option<i32>,
}

#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct CallSignal {
    pub call_id: String,
    pub action: CallAction,
    pub video: bool,
    pub sdp: Option<String>,
    pub candidates: Vec<IceCandidate>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, uniffi::Record)]
#[serde(default)]
pub struct Settings {
    pub read_receipts: bool,
    pub typing_indicators: bool,
    pub default_disappearing_secs: u32,
    pub notifications: bool,
}

impl Default for Settings {
    fn default() -> Self {
        Settings {
            read_receipts: true,
            typing_indicators: true,
            default_disappearing_secs: 0,
            notifications: true,
        }
    }
}

/// 核心推送给平台层的事件。
#[derive(Debug, Clone, PartialEq, uniffi::Enum)]
pub enum CoreEvent {
    ConnectionChanged { state: ConnectionState },
    ConversationsChanged,
    /// `backlog`：来自连接建立后、`queue_empty` 之前推送的积压，或 HTTP 补拉（平台据此不弹实时横幅）。
    /// `mentions_me`（v3 / K6）：收到的消息提及了我（会话免打扰时平台也应通知）。
    MessageAdded { conv_id: String, message: MessageItem, backlog: bool, mentions_me: bool },
    MessageUpdated { conv_id: String, message: MessageItem },
    MessageRemoved { conv_id: String, message_id: String },
    Typing { conv_id: String, user_id: String, typing: bool },
    FriendRequestReceived { request: FriendRequest },
    FriendsChanged,
    GroupsChanged,
    CallSignalReceived { from_user_id: String, signal: CallSignal },
    MomentNotification { notification: MomentNotification },
    KeyChanged { user_id: String },
    LoggedOut { reason: String },
}

/// 平台层实现的事件回调。
#[uniffi::export(foreign)]
pub trait EventListener: Send + Sync {
    fn on_event(&self, event: CoreEvent);
}
