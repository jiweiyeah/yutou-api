# KPay 移动端原生支付适配方案

## 1. 背景与现状

### 1.1 当前接入方式

系统设置 → epay（易支付）配置中填写 KPay 的 URL、PID、密钥，复用易支付协议接入 KPay 网关。

**调用链路：**
```
前端 usePayment.processPayment()
  → POST /api/user/pay (RequestEpay)
  → epay.Client.Purchase() 拼接 /submit.php 表单参数
  → 返回 {url: "https://kpay域名/submit.php", params: {...}}
  → 前端 submitPaymentForm() 构造隐藏 <form> POST 提交
  → 浏览器跳转到 KPay 收银台
```

### 1.2 问题

`controller/topup.go` 和 `controller/subscription_payment_epay.go` 中 `Device` 字段写死为 `epay.PC`：

```go
// controller/topup.go:233
Device: epay.PC,

// controller/subscription_payment_epay.go:106
Device: epay.PC,
```

导致移动端用户也走 PC 收银台流程，无法直接拉起支付宝/微信 App，体验差。

### 1.3 诉求

- 移动端：`device=mobile`，KPay 返回 H5 原生支付页，直接拉起支付宝/微信 App
- PC 端：保持现状，`device=pc`，走收银台扫码
- 兼容现有易支付接入，不引入新配置项

## 2. 方案设计

### 2.1 核心思路

后端按请求 User-Agent 动态选择 `epay.PC` / `epay.MOBILE`，前端零字段改动；移动端表单提交改为当前页跳转（避免拉起 App 后残留僵尸 tab）。

### 2.2 为什么不新增配置项

- `device` 本就是请求级参数，不是商户级配置
- 新增"PC/移动模式"开关会徒增后台复杂度，且与易支付协议语义不符
- UA 检测是确定性逻辑，无需运营人员介入

### 2.3 为什么不接 KPay 官方 API

KPay 官方文档（https://kpay.cc/api-docs）提供 `/pay/api/order/create` 接口，支持 `paymentMode=direct_qr` + `responseMode=alipay_jsapi` 返回 `alipays://` 深链，体验最干净。但：

- 需要 HMAC-SHA256 签名、nonce 防重放、IP 白名单等一整套机制
- 需要新建独立 controller、独立配置项（API Key / Secret）、独立前端入口
- 工作量大，且与现有易支付接入并行存在，维护成本高

当前方案在易支付协议基础上改动最小，依赖 KPay 易支付兼容层对 `device=mobile` 的支持（多数易支付网关均支持）。

## 3. 实施步骤

### 3.1 后端：新增 UA 检测工具函数

**文件：** `controller/topup.go`

在 `GetEpayClient()` 函数附近新增：

```go
// detectEpayDevice 根据请求 User-Agent 判断设备类型
// 移动端返回 epay.MOBILE（H5 原生支付），PC 返回 epay.PC（收银台扫码）
func detectEpayDevice(c *gin.Context) epay.DeviceType {
	ua := c.Request.UserAgent()
	if ua == "" {
		return epay.PC
	}
	// 移动端特征关键词（iPad 归 PC，按桌面扫码流程处理）
	mobileKeywords := []string{"Mobile", "Android", "iPhone", "iPod", "Windows Phone"}
	for _, kw := range mobileKeywords {
		if strings.Contains(ua, kw) {
			return epay.MOBILE
		}
	}
	return epay.PC
}
```

**注意：** 需要在 `topup.go` 的 import 块中新增 `"strings"`。

### 3.2 后端：普通充值替换 Device

**文件：** `controller/topup.go`

`RequestEpay` 函数中（约 233 行）：

```go
// 修改前
Device: epay.PC,

// 修改后
Device: detectEpayDevice(c),
```

### 3.3 后端：订阅支付替换 Device

**文件：** `controller/subscription_payment_epay.go`

`SubscriptionRequestEpay` 函数中（约 106 行）：

```go
// 修改前
Device: epay.PC,

// 修改后
Device: detectEpayDevice(c),
```

`detectEpayDevice` 定义在 `controller` 包内，`subscription_payment_epay.go` 同属该包，可直接调用，无需额外 import。

### 3.4 前端：移动端表单提交改为当前页跳转

**文件：** `web/default/src/features/wallet/lib/payment.ts`

`submitPaymentForm` 函数中，移动端不再开新 tab：

```typescript
// 修改前
export function submitPaymentForm(
  url: string,
  params: Record<string, unknown>
): void {
  const form = document.createElement('form')
  form.action = url
  form.method = 'POST'

  // Don't open in new tab for Safari
  if (!isSafariBrowser()) {
    form.target = '_blank'
  }
  // ...
}

// 修改后
export function submitPaymentForm(
  url: string,
  params: Record<string, unknown>
): void {
  const form = document.createElement('form')
  form.action = url
  form.method = 'POST'

  // 移动端当前页跳转（拉起 App 后避免残留僵尸 tab）
  // PC 端非 Safari 开新 tab，Safari 当前页（兼容 Safari 跨页提交限制）
  const isMobile = /Mobile|Android|iPhone|iPod|Windows Phone/i.test(navigator.userAgent)
  if (!isMobile && !isSafariBrowser()) {
    form.target = '_blank'
  }
  // ...
}
```

**说明：** UA 检测规则与后端 `detectEpayDevice` 保持一致，确保前后端判定同步。

### 3.5 改动清单汇总

| 文件 | 改动类型 | 说明 |
|------|----------|------|
| `controller/topup.go` | 新增函数 + 替换 1 处 | 新增 `detectEpayDevice`，`RequestEpay` 中 `Device` 改为动态 |
| `controller/subscription_payment_epay.go` | 替换 1 处 | `SubscriptionRequestEpay` 中 `Device` 改为动态 |
| `web/default/src/features/wallet/lib/payment.ts` | 修改条件 | `submitPaymentForm` 移动端当前页跳转 |

**不改动：**
- epay SDK（`github.com/Calcium-Ion/go-epay`）
- 路由、配置项、数据库
- 前端 API 调用层（`web/default/src/features/wallet/api.ts`）
- 前端充值入口组件（`web/default/src/features/wallet/index.tsx`）

## 4. 验证方案

### 4.1 后端单元验证

手动验证 UA 检测逻辑（不新增测试文件，遵循"不为覆盖率加测试"原则）：

| User-Agent | 期望 Device |
|------------|-------------|
| `Mozilla/5.0 (iPhone; CPU iPhone OS 17_0...)` | `epay.MOBILE` |
| `Mozilla/5.0 (Linux; Android 13...)` | `epay.MOBILE` |
| `Mozilla/5.0 (iPad; CPU OS 17_0...)` | `epay.PC` |
| `Mozilla/5.0 (Windows NT 10.0; Win64...)` | `epay.PC` |
| `Mozilla/5.0 (Macintosh; Intel Mac OS X...)` | `epay.PC` |
| `""`（空 UA） | `epay.PC` |

### 4.2 端到端验证

| 场景 | 设备 | 预期行为 |
|------|------|----------|
| 普通充值 | PC 浏览器 | 新 tab 打开 KPay 收银台，扫码支付 |
| 普通充值 | 手机浏览器 | 当前页跳转，拉起支付宝/微信 App |
| 普通充值 | iPad Safari | 新 tab 打开收银台（归 PC） |
| 订阅支付 | PC 浏览器 | 新 tab 打开收银台 |
| 订阅支付 | 手机浏览器 | 当前页跳转，拉起 App |
| Safari（任意端） | Mac Safari | 当前页跳转（保持原逻辑） |

### 4.3 回归验证

- PC 端充值流程与改动前完全一致（`device=pc` 不变）
- KPay 回调通知（`/api/user/epay/notify`）不受影响
- 订阅支付回调（`/api/subscription/epay/notify`）不受影响

## 5. 风险与兜底

### 5.1 KPay 易支付兼容层不支持 `device=mobile`

**风险：** KPay 易支付兼容接口可能忽略 `device` 参数，移动端仍返回 PC 收银台。

**兜底：** 最坏情况是移动端体验与改动前一致（走 PC 收银台），不会比现在更差。若验证后发现此问题，再考虑升级到方案 B（接 KPay 官方 API）。

### 5.2 UA 检测误判

**风险：** 某些边缘 UA（如桌面端触屏设备、浏览器隐私模式 stripped UA）可能误判。

**兜底：** 误判为 PC 时移动端走收银台（可用，体验略差）；误判为移动端时 PC 走 H5（多数易支付 H5 页也支持扫码）。两种误判均不会导致支付失败。

### 5.3 iPad 归 PC

**说明：** iPad Safari UA 含 "iPad"，本方案判定为 PC，走收银台扫码流程。如后续希望 iPad 拉起 App，可向 `mobileKeywords` 加回 "iPad"。

## 6. 不在本次范围内

- 接入 KPay 官方 API（`/pay/api/order/create` + HMAC 签名）
- KPay 退款、结算接口
- KPay 收银台自定义样式
- 其他支付通道（Stripe / Waffo / Creem）的移动端适配
