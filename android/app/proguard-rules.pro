# Hush release 构建的 R8 规则（isMinifyEnabled = true）

-keepattributes Signature,InnerClasses,EnclosingMethod,*Annotation*

# ---- UniFFI 生成的核心库绑定（src/main/java/uniffi）----
# 绑定通过 JNA 反射调用：UniffiLib 的 native 方法按方法名注册到 libhushcore.so，
# RustBuffer 等 Structure 子类按字段名/字段顺序映射内存，回调接口（EventListener 等）由 JNA 通过反射实现。
-keep class uniffi.hushcore.** { *; }
-keep interface uniffi.hushcore.** { *; }

# ---- JNA ----
-keep class com.sun.jna.** { *; }
-keep class * implements com.sun.jna.** { *; }
-keepclassmembers class * extends com.sun.jna.Structure { <fields>; <init>(...); }
-dontwarn java.awt.**
-dontwarn com.sun.jna.**

# ---- WebRTC（JNI 按类名/方法名回调 Java 层）----
-keep class org.webrtc.** { *; }
-dontwarn org.webrtc.**
# libjingle_peerconnection_so 在 JNI_OnLoad 中按类名查找 jni_zero 的注册类
-keep class org.jni_zero.** { *; }
-dontwarn org.jni_zero.**

# ---- ML Kit（扫一扫）----
# ML Kit 内置的组件发现机制按类名反射调用各 Registrar 的无参构造，否则启动时报 NoSuchMethodException、扫码不可用
-keep class * implements com.google.firebase.components.ComponentRegistrar { <init>(); }
-keep class com.google.mlkit.**.*Registrar { <init>(); }

# ---- 其他 ----
# kotlinx-coroutines、Compose、CameraX、ML Kit、Coil 自带 consumer 规则。
