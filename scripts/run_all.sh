#!/bin/bash

# =====================================================
# 快速执行指南 - 在服务器上运行此脚本即可完成所有操作
# =====================================================

echo "=========================================="
echo "yutou-api 渠道模型更新 - 快速执行"
echo "=========================================="
echo ""
echo "本脚本将依次执行："
echo "  1. 检查数据库状态"
echo "  2. 备份 channels 表"
echo "  3. 更新 models 字段"
echo "  4. 更新 model_mapping 字段"
echo "  5. 验证更新结果"
echo ""
read -p "按 Enter 继续，或 Ctrl+C 取消..."

# 获取脚本所在目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 执行检查脚本
echo ""
echo "=========================================="
echo "步骤 1: 检查数据库状态"
echo "=========================================="
bash "$SCRIPT_DIR/check_database_before_update.sh"

echo ""
read -p "检查结果如上所示。确认无误后按 Enter 继续更新，或 Ctrl+C 取消..."

# 执行更新脚本
echo ""
echo "=========================================="
echo "步骤 2: 执行更新"
echo "=========================================="
bash "$SCRIPT_DIR/smart_update_kimi.sh"

echo ""
echo "=========================================="
echo "✓ 所有操作完成！"
echo "=========================================="
echo ""
echo "⚠️  最后一步：重建 abilities 表"
echo ""
echo "请选择以下方式之一："
echo ""
echo "方式 1（推荐）: 管理后台"
echo "  1. 打开浏览器访问管理后台"
echo "  2. 进入「渠道管理」"
echo "  3. 点击「修复数据库一致性」按钮"
echo ""
echo "方式 2: API 调用"
echo "  curl -X POST http://your-domain/api/channel/fix \\"
echo "       -H 'Authorization: Bearer YOUR_ADMIN_TOKEN'"
echo ""
echo "完成后，新模型 moonshotai/kimi-k2.7-code 即可使用！"
echo ""
