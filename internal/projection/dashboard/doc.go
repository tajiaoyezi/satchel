// Package dashboard 是投影层的管理员仪表盘实时推送 WebSocket（GET /api/v1/ws/dashboard，node-telemetry）：
// 复用 authn 身份校验（拒绝 ?token= 查询参数）、对浏览器会话 Cookie 强制同源 Origin 防 CSWSH，
// 并在有管理员在线连接时按 dashboard_refresh_interval_ms 运行惰性广播循环，替代高频轮询。
package dashboard
