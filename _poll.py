import io

p = "src/web/index.html"
h = io.open(p, encoding="utf-8").read()

# 1) 替换旧的条件轮询
old1 = "    if(syncCache.some(t => t.state && t.state.running)){ setTimeout(loadSyncTasks, 1000); } // 同步中每秒刷新，完成即显示结果"
new1 = "    if(syncTimer) clearTimeout(syncTimer);\n    syncTimer = setTimeout(syncPollTick, 1500);"
assert old1 in h, "old1"
h = h.replace(old1, new1, 1)
io.open(p, "w", encoding="utf-8").write(h)
print("step1 ok")

# 2) 前面插入 syncPollTick 心跳函数
old2 = "/* ---------- 网盘同步（应用自带的独立 nginx 实例） ---------- */"
poll_fn = """function syncPollTick(){
  const pg = document.getElementById('page-sync');
  if(pg && pg.classList.contains('active')) {
    loadSyncTasks(); // 末尾会再次设置 syncTimer，形成轮询链
    return;
  }
  syncTimer = setTimeout(syncPollTick, 1500); // 已切到其他页：挂着等，切回即恢复
}

""" + old2
h = io.open(p, encoding="utf-8").read()
assert old2 in h, "old2"
h = h.replace(old2, poll_fn, 1)
io.open(p, "w", encoding="utf-8").write(h)
print("step2 ok")
