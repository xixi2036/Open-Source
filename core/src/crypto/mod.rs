//! 密码学相关模块。全部基于 vodozemac（Olm/Megolm）与 RustCrypto（aes-gcm、sha2），不自行实现原语。

pub mod attachment;
pub mod fieldenc;
pub mod megolm;
pub mod olm;
pub mod safety;
