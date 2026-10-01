import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

// 版本号可由 gradle 属性传入（CI 用构建号）：./gradlew assembleRelease -PversionCode=42 -PversionName=0.2.0
val appVersionCode = providers.gradleProperty("versionCode").map { it.toInt() }.getOrElse(1)
val appVersionName = providers.gradleProperty("versionName").getOrElse("0.1.0")
// 没有 keystore.properties 时 release 构建失败；本地验证用 -PallowDebugSigning=true 显式允许 debug 签名
val allowDebugSigning = providers.gradleProperty("allowDebugSigning").map { it.toBoolean() }.getOrElse(false)
val hasReleaseKeystore = rootProject.file("keystore.properties").exists()

android {
    namespace = "com.hush.app"
    // compileSdk 37 / targetSdk 36：用新 SDK 编译以获得新 API 与 lint 检查，运行时行为仍按已在
    // Android 16（API 36）上验证过的 targetSdk 36。升到 37 需要在 API 37 设备上回归（本机没有该系统镜像），暂不对齐。
    compileSdk = 37

    defaultConfig {
        applicationId = "com.hush.app"
        minSdk = 26
        targetSdk = 36
        versionCode = appVersionCode
        versionName = appVersionName
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    signingConfigs {
        // 独立的 release 签名：从 android/keystore.properties 读取（已加入 .gitignore，不入库）。
        // 内容示例：
        //   storeFile=/绝对路径/hush-release.jks
        //   storePassword=...
        //   keyAlias=hush
        //   keyPassword=...
        // 文件不存在时 release 构建失败（checkReleaseSigning），避免把 debug 签名的包当正式版发出去；
        // 只做本地验证时加 -PallowDebugSigning=true。
        val props = rootProject.file("keystore.properties")
        if (props.exists()) {
            val p = Properties().apply { props.inputStream().use { load(it) } }
            create("release") {
                storeFile = file(p.getProperty("storeFile"))
                storePassword = p.getProperty("storePassword")
                keyAlias = p.getProperty("keyAlias")
                keyPassword = p.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        debug {
            // 模拟器（x86_64 / arm64）与真机都能装
            ndk { abiFilters += listOf("arm64-v8a", "x86_64") }
            buildConfigField("String", "DEFAULT_SERVER", "\"http://10.0.2.2:8080\"")
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // C5：正式版只打 arm64-v8a（x86_64 的 so 约 12.8MB，真机用不到）
            ndk { abiFilters += listOf("arm64-v8a") }
            // 正式版不预置服务器地址（模拟器地址对真机没有意义），由用户填写自己部署的地址
            buildConfigField("String", "DEFAULT_SERVER", "\"\"")
            signingConfig = signingConfigs.findByName("release")
                ?: if (allowDebugSigning) signingConfigs.getByName("debug") else null
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        compose = true
        buildConfig = true
    }
    // C5：不再使用 useLegacyPackaging。so 以未压缩、按页对齐的方式留在 APK 内由系统直接映射
    // （extractNativeLibs=false），安装后不再解压一份到 data 分区；JNA 的 libjnidispatch.so 与 libhushcore.so
    // 都能从 APK 内加载（release 包已在模拟器上实测启动、登录、收发消息）。
    lint {
        // 生成的 UniFFI 绑定（src/main/java/uniffi）的豁免见 app/lint.xml
        lintConfig = file("lint.xml")
        // release 只打 arm64-v8a 是有意的（C5）；不面向 ChromeOS x86_64 设备
        disable += "ChromeOsAbiSupport"
    }
}

/**
 * UniFFI 生成的 HushException 各变体同时声明了构造参数 `message` 与 `override val message`，
 * Kotlin K2 报 REDECLARATION。这里在编译前幂等地把构造参数改为 `override val` 并删去重复的 getter。
 * 绑定文件由 scripts/build-core-android.sh 重新生成后会被自动再次修补。详见 docs/issues/android.md。
 */
val patchUniffiBindings by tasks.registering {
    val binding = file("src/main/java/uniffi/hushcore/hushcore.kt")
    outputs.upToDateWhen { false }
    doLast {
        if (!binding.exists()) return@doLast
        val src = binding.readText()
        val regex = Regex(
            """(\n[ \t]*)val `message`: kotlin\.String(\s*\) : HushException\(\) \{)\s*override val message\s*get\(\) = "[^"\n]*"""",
        )
        val patched = regex.replace(src) { m -> "${m.groupValues[1]}override val `message`: kotlin.String${m.groupValues[2]}" }
        if (patched != src) binding.writeText(patched)
    }
}
tasks.named("preBuild") { dependsOn(patchUniffiBindings) }

/**
 * release 没有签名配置时让构建失败（而不是悄悄用 debug 签名或产出未签名包）。
 * 在任务执行阶段检查，不影响 assembleDebug / lint / 单元测试。
 */
val checkReleaseSigning by tasks.registering {
    val ok = hasReleaseKeystore || allowDebugSigning
    doFirst {
        if (!ok) {
            throw GradleException(
                "release 构建需要签名：请提供 android/keystore.properties（storeFile/storePassword/keyAlias/keyPassword）。" +
                    "仅本地验证可加 -PallowDebugSigning=true 使用 debug 签名（不能发布）。",
            )
        }
    }
}
tasks.configureEach { if (name == "preReleaseBuild") dependsOn(checkReleaseSigning) }

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.process)
    implementation(libs.androidx.navigation.compose)
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.compose.material3)
    implementation(libs.compose.material.icons)
    debugImplementation(libs.compose.ui.tooling)
    implementation(libs.kotlinx.coroutines.android)
    implementation("${libs.jna.get().module}:${libs.jna.get().version}@aar")
    implementation(libs.coil.compose)
    implementation(libs.webrtc)
    implementation(libs.camerax.camera2)
    implementation(libs.camerax.lifecycle)
    implementation(libs.camerax.view)
    implementation(libs.mlkit.barcode)
    implementation(libs.zxing.core)
    implementation(libs.androidx.biometric)
    implementation(libs.androidx.exifinterface)
    implementation(libs.coil.gif)
    testImplementation(libs.junit)
    androidTestImplementation(platform(libs.compose.bom))
    androidTestImplementation(libs.compose.ui.test.junit4)
    androidTestImplementation(libs.androidx.test.runner)
    androidTestImplementation(libs.androidx.test.ext.junit)
    debugImplementation(libs.compose.ui.test.manifest)
}
