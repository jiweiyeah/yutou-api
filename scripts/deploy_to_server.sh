#!/bin/bash
# 一键打包所有脚本到服务器

SERVER="root@69.165.75.53"
REMOTE_DIR="/tmp/yutou-update"

echo "打包并上传脚本到服务器..."

# 创建远程目录
ssh $SERVER "mkdir -p $REMOTE_DIR"

# 上传所有脚本
scp scripts/check_database_before_update.sh $SERVER:$REMOTE_DIR/
scp scripts/smart_update_kimi.sh $SERVER:$REMOTE_DIR/
scp scripts/run_all.sh $SERVER:$REMOTE_DIR/
scp scripts/COMPLETE_GUIDE.md $SERVER:$REMOTE_DIR/

echo ""
echo "✓ 上传完成！"
echo ""
echo "接下来在服务器上执行："
echo ""
echo "  ssh $SERVER"
echo "  cd $REMOTE_DIR"
echo "  bash run_all.sh"
echo ""
