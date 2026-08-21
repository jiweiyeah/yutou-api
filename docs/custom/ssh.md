# 生产 SSH 连接（自定义）

本文只记录 **yutou-api 生产机** 的登录方式。私钥 **禁止** 提交到 git、禁止贴进 Issue/PR/聊天。

| 项 | 值 |
|----|----|
| Host | `154.202.119.148` |
| User | `root` |
| Port | **58317**（不是 22） |
| 认证 | **仅公钥**，密码已关 |
| 业务目录 | `/opt/new-api/` |

GitHub Actions 部署走同一端口、同一 `root` 用户，密钥是 **另一把** deploy key（repo secret `DEPLOY_SSH_KEY` + `DEPLOY_PORT=58317`）。个人笔记本不要复用那把 CI 私钥。

---

## 1. 当前这台电脑怎么连

本机已有个人密钥 `~/.ssh/yutou_prod` 时：

```sshconfig
# ~/.ssh/config
Host yutou-prod
    HostName 154.202.119.148
    User root
    Port 58317
    IdentityFile ~/.ssh/yutou_prod
    IdentitiesOnly yes
    ServerAliveInterval 30
    ServerAliveCountMax 3
```

```bash
ssh yutou-prod
```

等价于：

```bash
ssh -i ~/.ssh/yutou_prod -o IdentitiesOnly=yes -p 58317 root@154.202.119.148
```

服务器 `authorized_keys` 里应能看到你的公钥（comment 类似 `yjw@laptop-yutou-prod`）。不要删另一行 `github-actions-deploy@yutou-api`，删了 CI 部署会断。

---

## 2. 换一台设备：怎么拿到能登录的密钥

**没有「从仓库拉取私钥」这回事。** 仓库里只有这份说明。换电脑按下面三条选一条。

### 路径 A（推荐）：新电脑生成新密钥，用旧电脑写进去

旧电脑还能 `ssh yutou-prod` 时用这条。每台设备一把自己的密钥，丢一台只吊销一台。

**新电脑：**

```bash
ssh-keygen -t ed25519 -f ~/.ssh/yutou_prod -C "yourname@new-laptop-yutou-prod"
cat ~/.ssh/yutou_prod.pub
# 把这一行完整拷到旧电脑
```

把上一节的 `Host yutou-prod` 写进新电脑的 `~/.ssh/config`。

**旧电脑（已能登录的那台）：**

```bash
ssh yutou-prod 'mkdir -p /root/.ssh && chmod 700 /root/.ssh'
# 把新电脑的 .pub 整行追加进去（用引号包住，不要手打漏字符）
ssh yutou-prod "echo 'ssh-ed25519 AAAA... yourname@new-laptop-yutou-prod' >> /root/.ssh/authorized_keys"
ssh yutou-prod 'chmod 600 /root/.ssh/authorized_keys && cat /root/.ssh/authorized_keys'
```

**新电脑验证：**

```bash
ssh -o BatchMode=yes yutou-prod 'echo OK'
```

通了以后，旧电脑退役时再从 `authorized_keys` 删掉旧公钥那一行。

### 路径 B：旧电脑还在，把「个人私钥」拷到新电脑

只拷 **个人** 密钥 `~/.ssh/yutou_prod`（+ `.pub`），不要拷 `~/.ssh/yutou-deploy/`（那是 CI 用的）。

用 U 盘、1Password SSH agent、或加密压缩包，**不要** 用微信/邮箱明文传。

新电脑：

```bash
mkdir -p ~/.ssh && chmod 700 ~/.ssh
# 放入 yutou_prod / yutou_prod.pub
chmod 600 ~/.ssh/yutou_prod
chmod 644 ~/.ssh/yutou_prod.pub
# 写入与第 1 节相同的 Host yutou-prod
ssh yutou-prod
```

两台电脑共享同一把个人密钥也可以，但丢/泄露后必须立刻在服务器删公钥并换新钥。路径 A 更干净。

### 路径 C：旧电脑没了、私钥也没备份

密码登录已经关掉，**GitHub 上的 `DEPLOY_SSH_KEY` 也不能当个人登录用**（可以救 CI，但不能替代你本机密钥，除非你把 CI 私钥下到笔记本——不建议）。

此时只能走 **云厂商救援 / VNC / IPMI**（这台是 KVM 虚机，找供应商面板的 VNC）：

1. 在 **新电脑** 生成密钥（同路径 A 的 `ssh-keygen`），复制 `.pub` 那一行。
2. VNC 登进服务器（本地控制台，不走 SSH）。
3. 执行：

```bash
mkdir -p /root/.ssh && chmod 700 /root/.ssh
echo 'ssh-ed25519 AAAA... yourname@new-laptop-yutou-prod' >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
# 确认 sshd 仍在 58317、密码仍关闭
sshd -T | grep -E '^(port |passwordauthentication |permitrootlogin )'
```

4. 新电脑 `ssh yutou-prod` 成功后，**立刻** 检查 `authorized_keys`：只留你信任的个人公钥 + `github-actions-deploy@yutou-api` 那一行。旧电脑的公钥删掉。

若救援时需要临时开 22 / 开密码：改完立刻关回去。备份文件：`/etc/ssh/sshd_config.bak.20260821-234154`，加固 drop-in：`/etc/ssh/sshd_config.d/99-harden.conf`。

---

## 3. 不要做的事

- 不要把 `yutou_prod`、`yutou-deploy/id_ed25519` 或 `DEPLOY_SSH_KEY` 的私钥内容提交进本仓库。
- 不要把 CI 的 deploy 私钥分发到多台笔记本。CI 只放在 GitHub Secrets。
- 不要用 `ssh-copy-id` 走 22 或密码——端口不是 22，密码已经关了。
- 不要从 `authorized_keys` 删除 `github-actions-deploy@yutou-api`，除非你已经轮换并更新了 `DEPLOY_SSH_KEY`。

---

## 4. 轮换 / 吊销

某台电脑丢失：

```bash
ssh yutou-prod
# 编辑 /root/.ssh/authorized_keys，删掉那台机器对应的一行
```

轮换 CI 密钥：先把 **新** 公钥写入 `authorized_keys` → 更新 GitHub secret `DEPLOY_SSH_KEY` → 跑一次 `custom-deploy.yml` 成功 → 再删旧公钥。顺序反了部署会断。
