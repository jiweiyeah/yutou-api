#!/bin/bash

# =====================================================
# 数据库检查脚本 - 执行更新前的数据审查
# =====================================================

set -e

DB_USER=${DB_USER:-root}
DB_NAME=${DB_NAME:-one_api}

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${GREEN}=========================================="
echo "数据库检查脚本"
echo -e "==========================================${NC}"
echo ""

# 检测数据库类型
echo -e "${YELLOW}[1/6] 检测数据库类型...${NC}"

if command -v psql &> /dev/null; then
    echo -e "${BLUE}发现 PostgreSQL 客户端${NC}"
    DB_TYPE="postgres"
    DB_CMD="psql -U postgres -d one_api -t -A"
elif command -v mysql &> /dev/null; then
    echo -e "${BLUE}发现 MySQL 客户端${NC}"
    DB_TYPE="mysql"
    DB_PASS=${DB_PASS:?请通过环境变量 DB_PASS 提供数据库密码}
    DB_CMD="mysql -u $DB_USER -p****** $DB_NAME -sN"
else
    echo -e "${RED}错误: 未找到数据库客户端${NC}"
    exit 1
fi

echo -e "${GREEN}✓ 数据库类型: $DB_TYPE${NC}"
echo ""

# 函数：执行数据库查询
run_query() {
    if [ "$DB_TYPE" = "postgres" ]; then
        psql -U postgres -d one_api -t -A -c "$1"
    else
        MYSQL_PWD="$DB_PASS" mysql -u "$DB_USER" "$DB_NAME" -sN -e "$1"
    fi
}

# 检查 channels 表结构
echo -e "${YELLOW}[2/6] 检查 channels 表结构...${NC}"
if [ "$DB_TYPE" = "postgres" ]; then
    run_query "SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 'channels' AND column_name IN ('models', 'model_mapping') ORDER BY column_name;"
else
    run_query "SHOW COLUMNS FROM channels WHERE Field IN ('models', 'model_mapping');"
fi
echo ""

# 统计渠道总数
echo -e "${YELLOW}[3/6] 统计渠道总数...${NC}"
TOTAL_CHANNELS=$(run_query "SELECT COUNT(*) FROM channels;")
echo -e "${GREEN}✓ 总渠道数: $TOTAL_CHANNELS${NC}"
echo ""

# 检查已包含 kimi-k2.7-code 的渠道
echo -e "${YELLOW}[4/6] 检查已包含 kimi-k2.7-code 模型的渠道...${NC}"
EXISTING_MODEL=$(run_query "SELECT COUNT(*) FROM channels WHERE models LIKE '%moonshotai/kimi-k2.7-code%';")
echo -e "${BLUE}已包含该模型的渠道数: $EXISTING_MODEL${NC}"
echo ""

# 检查 models 字段的不同情况
echo -e "${YELLOW}[5/6] 分析 models 字段的数据情况...${NC}"
echo -e "${BLUE}空值或 NULL 的渠道数:${NC}"
run_query "SELECT COUNT(*) FROM channels WHERE models IS NULL OR models = '';"

echo -e "${BLUE}示例渠道数据 (前 5 个):${NC}"
if [ "$DB_TYPE" = "postgres" ]; then
    run_query "SELECT id, name, SUBSTRING(models, 1, 80) as models_preview, SUBSTRING(model_mapping, 1, 100) as mapping_preview FROM channels ORDER BY id LIMIT 5;" | column -t -s '|'
else
    MYSQL_PWD="$DB_PASS" mysql -u "$DB_USER" "$DB_NAME" -t -e "SELECT id, name, SUBSTRING(models, 1, 80) as models_preview, SUBSTRING(model_mapping, 1, 100) as mapping_preview FROM channels ORDER BY id LIMIT 5;"
fi
echo ""

# 检查 model_mapping 字段的情况
echo -e "${YELLOW}[6/6] 分析 model_mapping 字段的数据情况...${NC}"
echo -e "${BLUE}NULL 或空值的渠道数:${NC}"
run_query "SELECT COUNT(*) FROM channels WHERE model_mapping IS NULL OR model_mapping = '';"

echo -e "${BLUE}空 JSON 对象 {} 的渠道数:${NC}"
run_query "SELECT COUNT(*) FROM channels WHERE model_mapping = '{}';"

echo -e "${BLUE}已有映射规则的渠道数:${NC}"
run_query "SELECT COUNT(*) FROM channels WHERE model_mapping IS NOT NULL AND model_mapping != '' AND model_mapping != '{}';"

echo -e "${BLUE}已包含 kimi-k2.7-code 映射的渠道数:${NC}"
if [ "$DB_TYPE" = "postgres" ]; then
    run_query "SELECT COUNT(*) FROM channels WHERE model_mapping LIKE '%moonshotai/kimi-k2.7-code%';"
else
    run_query "SELECT COUNT(*) FROM channels WHERE model_mapping LIKE '%moonshotai/kimi-k2.7-code%';"
fi
echo ""

# 检查 abilities 表
echo -e "${YELLOW}[bonus] 检查 abilities 表...${NC}"
TOTAL_ABILITIES=$(run_query "SELECT COUNT(*) FROM abilities;")
echo -e "${GREEN}✓ 当前 abilities 记录数: $TOTAL_ABILITIES${NC}"

DISTINCT_MODELS=$(run_query "SELECT COUNT(DISTINCT model) FROM abilities;")
echo -e "${GREEN}✓ abilities 中的不同模型数: $DISTINCT_MODELS${NC}"

KIMI_IN_ABILITIES=$(run_query "SELECT COUNT(*) FROM abilities WHERE model = 'moonshotai/kimi-k2.7-code';")
echo -e "${BLUE}abilities 中已有 kimi-k2.7-code 的记录数: $KIMI_IN_ABILITIES${NC}"
echo ""

echo -e "${GREEN}=========================================="
echo "✓ 数据库检查完成！"
echo -e "==========================================${NC}"
echo ""

# 生成建议
echo -e "${YELLOW}📊 更新影响预估:${NC}"
NEED_MODEL_UPDATE=$((TOTAL_CHANNELS - EXISTING_MODEL))
echo "- 需要添加模型的渠道数: $NEED_MODEL_UPDATE"
echo "- 已有该模型的渠道数: $EXISTING_MODEL (将跳过)"
echo ""

if [ $TOTAL_ABILITIES -gt 0 ]; then
    echo -e "${RED}⚠️  重要提醒:${NC}"
    echo "当前 abilities 表有 $TOTAL_ABILITIES 条记录"
    echo "更新 channels 后必须重建 abilities 表以保持一致性"
    echo ""
fi

echo -e "${YELLOW}💡 abilities 表说明:${NC}"
echo "abilities 表是模型调度的核心索引表，作用是："
echo "1. 存储每个渠道支持的所有模型（从 channels.models 展开）"
echo "2. 记录每个模型在不同分组中的可用性（group + model + channel_id）"
echo "3. 提供快速的模型匹配查询（通过索引快速找到可用渠道）"
echo "4. 支持优先级和权重调度（priority, weight 字段）"
echo ""
echo "为什么要重建："
echo "- channels.models 是逗号分隔的字符串（如 'gpt-4,claude-3'）"
echo "- abilities 将其展开为独立记录（每个模型一条记录）"
echo "- 更新 channels.models 后，abilities 不会自动同步"
echo "- 必须调用 FixAbility() 函数删除旧记录并重新生成"
echo ""

echo -e "${GREEN}准备就绪！可以执行更新脚本。${NC}"
