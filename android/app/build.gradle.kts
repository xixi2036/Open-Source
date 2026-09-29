import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

android {
    namespace = "com.hush.app"
    compileSdk = 37

    defaultConfig {
        applicationId = "com.hush.app"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
        ndk { abiFilters += listOf("arm64-v8a", "x86_64") }
    }

    signingConfigs {
        // 独立的 release 签名：从 android/keystore.properties 读取（已加入 .gitignore，不入库）。
        // 内容示例：
        //   storeFile=/绝对路径/hush-release.jks
        //   storePassword=...
        //   keyAlias=hush
        //   keyPassword=...
        // 文件不存在时 release 回退使用 debug 签名（仅用于本地验证，不能发布）。
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
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            signingConfig = signingConfigs.findByName("release") ?: signingConfigs.getByName("debug")
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
    packaging { jniLibs { useLegacyPackaging = true } }
    lint {
        // 生成的 UniFFI 绑定（src/main/java/uniffi）的豁免见 app/lint.xml
        lintConfig = file("lint.xml")
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
    testImplementation(libs.junit)
}
