# 动态出口 IP 切换 - 部署清单

## 📦 本次实现的所有文件

### 核心代码（Go）

```
relay/proxy/
├── degraded_manager.go    # 7.8KB - 核心降级管理器
│   ├── DegradedManager 结构体
│   ├── 状态机逻辑
│   ├── HTTP 客户端包装
│   └── 统计信息收集
│
└── helper.go              # 1.8KB - 辅助函数
    ├── WrapHTTPClientWithProxy()
    ├── RecordResponseForProxy()
    └── GetProxyStatsForAPI()

controller/
└── proxy_stats.go         # 988B - 管理端点
    ├── GetProxyStats()
    └── ResetProxyState()

relay/channel/
└── api_request.go         # 已修改 - 集成代理包装

controller/
└── relay.go               # 已修改 - 记录响应结果

router/
└── api-router.go          # 已修改 - 添加路由
    ├── GET  /api/option/proxy_stats/:id
    └── POST /api/option/proxy_stats/:id/reset
```

### 部署脚本

```
scripts/
├── setup-dynamic-proxy.sh    # 6.5KB - 自动部署脚本
│   ├── 安装 Xray 代理服务
│   ├── 配置 systemd 服务
│   └── 验证连通性
│
├── test-dynamic-proxy.sh     # 4.9KB - 自动化测试脚本
│   ├── 检查服务状态
│   ├── 验证 API 端点
│   └── 测试代理连通性
│
└── xray-proxy-config.json    # 3.6KB - Xray 配置模板
    ├── 5 个 Cloudflare IP 节点
    ├── SOCKS5 + HTTP 双协议
    └── 随机负载均衡
```

### 文档

```
docs/custom/
├── README-PROXY.md                              # 9.5KB - 总览文档
│   ├── 功能介绍
│   ├── 快速开始
│   ├── API 端点
│   └── FAQ
│
├── QUICK-DEPLOY-PROXY.md                        # 8.1KB - 快速部署指南
│   ├── 3 步部署流程
│   ├── 验证清单
│   └── 故障排查
│
├── dynamic-proxy-implementation-summary.md      # 12KB - 完整实现文档
│   ├── 架构设计
│   ├── 核心逻辑
│   ├── 性能优化
│   └── 运维指南
│
├── docker-compose-proxy-config.md               # 4.8KB - Docker 配置
│   ├── 网络模式选择
│   ├── 参数调优
│   └── 配置示例
│
├── dynamic-proxy-setup.md                       # 7.3KB - 配置指南
│   └── 详细配置说明
│
└── DEPLOYMENT-CHECKLIST.md                      # 本文件
    └── 部署清单
```

---

## ✅ 部署前检查清单

### 环境准备

- [ ] 生产服务器 SSH 访问权限
- [ ] 管理员 API Token
- [ ] Xray 已安装（或运行脚本自动安装）
- [ ] Docker 和 docker-compose 可用
- [ ] 有效的 Xray 订阅 URL

### 权限检查

- [ ] sudo 权限（安装 systemd 服务需要）
- [ ] docker-compose 文件写入权限
- [ ] 日志查看权限

---

## 📋 部署步骤清单

### 第一阶段：代码部署

- [ ] **1.1** 在本地编译代码
  ```bash
  cd /path/to/yutou-api
  go build -o new-api main.go
  ```
  **验证**: 文件大小约 140-150MB

- [ ] **1.2** 上传到生产服务器
  ```bash
  scp new-api yutou-prod:/path/to/deploy/
  ```
  **验证**: `ssh yutou-prod "ls -lh /path/to/deploy/new-api"`

- [ ] **1.3** 上传脚本文件
  ```bash
  scp scripts/setup-dynamic-proxy.sh yutou-prod:/path/to/deploy/
  scp scripts/test-dynamic-proxy.sh yutou-prod:/path/to/deploy/
  ```
  **验证**: 脚本文件存在且可执行

---

### 第二阶段：Xray 代理配置

- [ ] **2.1** SSH 登录生产服务器
  ```bash
  ssh yutou-prod
  ```

- [ ] **2.2** 运行部署脚本
  ```bash
  cd /path/to/deploy
  chmod +x setup-dynamic-proxy.sh
  sudo ./setup-dynamic-proxy.sh
  ```
  **预期输出**:
  ```
  ✓ xray-proxy 服务启动成功
  ✓ 代理工作正常，出口 IP: 104.17.219.106
  本机 IP: 154.202.119.148
  代理 IP: 104.17.219.106
  SOCKS5 代理: 127.0.0.1:10808
  HTTP 代理:   127.0.0.1:10809
  ```

- [ ] **2.3** 验证 xray-proxy 服务
  ```bash
  sudo systemctl status xray-proxy
  ```
  **预期**: `Active: active (running)`

- [ ] **2.4** 验证端口监听
  ```bash
  ss -tlnp | grep -E "10808|10809"
  ```
  **预期**: 两个端口都在监听

- [ ] **2.5** 测试代理连通性
  ```bash
  curl -x socks5://127.0.0.1:10808 https://api.ipify.org
  ```
  **预期**: 返回 Cloudflare IP（不是本机 IP）

---

### 第三阶段：Docker 配置

- [ ] **3.1** 备份现有配置
  ```bash
  cp docker-compose.yml docker-compose.yml.bak.$(date +%Y%m%d_%H%M%S)
  ```

- [ ] **3.2** 编辑 docker-compose.yml
  添加以下内容：
  ```yaml
  services:
    new-api:
      # ===== CUSTOM START: 动态代理切换 =====
      network_mode: "host"
      # ===== CUSTOM END =====
      
      environment:
        # ... 其他环境变量 ...
        
        # ===== CUSTOM START: 动态代理配置 =====
        BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
        BITDEER_DEGRADED_THRESHOLD: "5"
        BITDEER_RECOVERY_THRESHOLD: "10"
        BITDEER_QUEUE_DELAY_MS: "100-500"
        # ===== CUSTOM END =====
  ```

- [ ] **3.3** 验证配置语法
  ```bash
  docker-compose config > /dev/null
  ```
  **预期**: 无错误输出

---

### 第四阶段：服务部署

- [ ] **4.1** 停止现有容器
  ```bash
  docker-compose down
  ```
  **预期**: `Container new-api  Stopped`

- [ ] **4.2** 启动新容器
  ```bash
  docker-compose up -d
  ```
  **预期**: `Container new-api  Started`

- [ ] **4.3** 等待容器启动
  ```bash
  sleep 5
  ```

- [ ] **4.4** 检查容器状态
  ```bash
  docker ps | grep new-api
  ```
  **预期**: `STATUS = Up X seconds (healthy)`

- [ ] **4.5** 检查容器日志
  ```bash
  docker logs new-api 2>&1 | grep -E "DegradedManager|listening"
  ```
  **预期**: 看到 "DegradedManager initialized" 日志

---

### 第五阶段：功能验证

- [ ] **5.1** 运行自动化测试
  ```bash
  cd /path/to/deploy
  chmod +x test-dynamic-proxy.sh
  ./test-dynamic-proxy.sh YOUR_ADMIN_TOKEN
  ```
  **预期**: 所有测试通过

- [ ] **5.2** 手动验证渠道 10836
  ```bash
  curl 'http://localhost:3000/api/option/proxy_stats/10836' \
    -H 'Authorization: Bearer YOUR_TOKEN' | jq
  ```
  **预期响应**:
  ```json
  {
    "success": true,
    "data": {
      "enabled": true,
      "mode": "direct",
      "proxy_url": "socks5://127.0.0.1:10808"
    }
  }
  ```

- [ ] **5.3** 手动验证渠道 10835
  ```bash
  curl 'http://localhost:3000/api/option/proxy_stats/10835' \
    -H 'Authorization: Bearer YOUR_TOKEN' | jq
  ```
  **预期**: 与 10836 类似的响应

- [ ] **5.4** 测试重置功能
  ```bash
  curl -X POST 'http://localhost:3000/api/option/proxy_stats/10836/reset' \
    -H 'Authorization: Bearer YOUR_TOKEN' | jq
  ```
  **预期**: `{"success": true}`

---

### 第六阶段：监控设置

- [ ] **6.1** 设置日志监控
  ```bash
  # 添加到 crontab 或监控系统
  */5 * * * * docker logs new-api 2>&1 | grep "DegradedManager" >> /var/log/proxy-events.log
  ```

- [ ] **6.2** 配置告警（可选）
  - 当 `consecutive_429 >= 3` 时发送通知
  - 当 `mode = "proxy"` 持续 > 10 分钟时发送通知

- [ ] **6.3** 添加到监控面板（可选）
  - 添加 Grafana 面板展示代理状态
  - 添加 Prometheus 指标收集

---

## 🔍 验证清单

### 立即验证（部署后 5 分钟内）

- [ ] xray-proxy 服务运行正常
- [ ] new-api 容器健康
- [ ] API 端点返回正确响应
- [ ] `enabled: true` 且 `proxy_url` 不为空
- [ ] 初始模式为 `"direct"`
- [ ] 日志中有 "DegradedManager initialized" 消息

### 短期验证（部署后 1 小时内）

- [ ] 观察是否有 429 错误
- [ ] 如有 429，观察是否自动切换到 PROXY 模式
- [ ] 检查 `total_direct_reqs` 是否持续增长
- [ ] 检查 `total_proxy_reqs` 是否远小于 `total_direct_reqs`

### 长期验证（部署后 24 小时内）

- [ ] 统计降级事件次数
- [ ] 统计恢复事件次数
- [ ] 计算代理模式占比（应 < 10%）
- [ ] 检查是否有异常的频繁切换
- [ ] 对比部署前后的 429 错误率

---

## 🚨 故障回滚清单

如果部署出现问题，按以下步骤回滚：

### 快速回滚（保留代理服务）

- [ ] **R1** 恢复旧的 docker-compose.yml
  ```bash
  cp docker-compose.yml.bak.XXXXXXXX docker-compose.yml
  ```

- [ ] **R2** 重启容器
  ```bash
  docker-compose restart new-api
  ```

- [ ] **R3** 验证服务正常
  ```bash
  docker ps | grep new-api
  curl http://localhost:3000/api/status
  ```

### 完全回滚（移除所有更改）

- [ ] **R4** 停止 xray-proxy 服务
  ```bash
  sudo systemctl stop xray-proxy
  sudo systemctl disable xray-proxy
  ```

- [ ] **R5** 恢复旧代码
  ```bash
  # 使用之前备份的二进制文件
  cp new-api.bak.XXXXXXXX new-api
  ```

- [ ] **R6** 清理配置文件
  ```bash
  sudo rm -rf /usr/local/etc/xray-proxy
  sudo rm -f /etc/systemd/system/xray-proxy.service
  sudo systemctl daemon-reload
  ```

---

## 📊 成功标准

部署被认为成功的标准：

### 技术指标

- ✅ xray-proxy 服务 uptime > 99%
- ✅ API 端点响应时间 < 100ms
- ✅ 容器重启后自动恢复功能
- ✅ 代理模式占比 < 10%（正常情况下）

### 业务指标

- ✅ 429 错误率下降 > 50%（相比部署前）
- ✅ 用户请求成功率提升
- ✅ 无因代理导致的新故障
- ✅ 平均响应时间增加 < 10%

---

## 📝 部署后任务

- [ ] 更新运维文档
- [ ] 通知相关团队成员
- [ ] 在监控系统中添加新指标
- [ ] 设置告警规则
- [ ] 记录部署时间和版本号
- [ ] 添加到变更日志
- [ ] 安排一周后的回顾会议

---

## 📞 紧急联系

如果部署过程中遇到严重问题：

1. 立即执行快速回滚
2. 记录错误日志
3. 通知技术负责人
4. 提交详细的问题报告

---

## 📚 相关文档

- [README-PROXY.md](./README-PROXY.md) - 功能总览
- [QUICK-DEPLOY-PROXY.md](./QUICK-DEPLOY-PROXY.md) - 快速部署指南
- [dynamic-proxy-implementation-summary.md](./dynamic-proxy-implementation-summary.md) - 完整实现文档
- [docker-compose-proxy-config.md](./docker-compose-proxy-config.md) - Docker 配置详解

---

**部署日期**: _______________
**部署人员**: _______________
**验证人员**: _______________
**备注**: _______________
