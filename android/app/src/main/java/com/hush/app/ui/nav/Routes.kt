package com.hush.app.ui.nav

import android.net.Uri

/** 导航路由（字符串路由，参数统一 URL 编码）。 */
object Routes {
    const val MAIN = "main"
    const val CHAT = "chat/{convId}?msg={msg}"
    const val CHAT_SETTINGS = "chatSettings/{convId}"
    const val SAFETY = "safety/{userId}"
    const val IMAGE = "image?path={path}"
    const val ARCHIVED = "archived"
    const val SEARCH = "search"
    const val NEW_CHAT = "newChat"
    const val NEW_GROUP = "newGroup"
    const val ADD_MEMBERS = "addMembers/{groupId}"
    const val FRIEND_REQUESTS = "friendRequests"
    const val GROUPS = "groups"
    const val PROFILE = "profile/{userId}"
    const val ADD_FRIEND = "addFriend"
    const val MY_QR = "myQr"
    const val SCAN = "scan?safetyUser={safetyUser}"
    const val COMPOSE_MOMENT = "composeMoment"
    const val MOMENT_NOTIFS = "momentNotifs"
    const val USER_MOMENTS = "userMoments/{userId}"
    const val MOMENT = "moment/{momentId}"
    const val EDIT_PROFILE = "editProfile"
    const val BLOCKED = "blocked"
    const val ABOUT = "about"

    private fun enc(s: String) = Uri.encode(s)

    fun chat(convId: String, messageId: String? = null) = if (messageId == null) "chat/${enc(convId)}" else "chat/${enc(convId)}?msg=${enc(messageId)}"
    fun chatSettings(convId: String) = "chatSettings/${enc(convId)}"
    fun safety(userId: String) = "safety/${enc(userId)}"
    fun image(path: String) = "image?path=${enc(path)}"
    fun addMembers(groupId: String) = "addMembers/${enc(groupId)}"
    fun profile(userId: String) = "profile/${enc(userId)}"
    fun scan(safetyUser: String? = null) = if (safetyUser == null) "scan" else "scan?safetyUser=${enc(safetyUser)}"
    fun userMoments(userId: String) = "userMoments/${enc(userId)}"
    fun moment(momentId: String) = "moment/${enc(momentId)}"
}
