import io

p = "src/web/index.html"
h = io.open(p, encoding="utf-8").read()

# 1) XHR：status=0/5xx 视为传输层错误（允许换通道重试），不再直接弹 HTTP 0
old = """        }else{
          let msg = 'HTTP ' + xhr.status;
          try{ const j = JSON.parse(xhr.responseText); if(j && j.error) msg = j.error; }catch(e){}
          reject(new Error(msg)); // 服务端有响应：应用错误，不换通道
        }"""
new = """        }else if(xhr.status === 0 || xhr.status >= 500){
          // status=0（连接被中断/网关忙）与 5xx（过载）是传输层问题：
          // 标记 transport 让上层换通道重试，而不是当成应用错误弹「HTTP 0」。
          let msg = 'HTTP ' + xhr.status;
          try{ const j = JSON.parse(xhr.responseText); if(j && j.error) msg = j.error; }catch(e){}
          const e = new Error(msg); e.transport = true; reject(e);
        }else{
          let msg = 'HTTP ' + xhr.status;
          try{ const j = JSON.parse(xhr.responseText); if(j && j.error) msg = j.error; }catch(e){}
          reject(new Error(msg)); // 其他 4xx：应用错误，不换通道
        }"""
assert old in h, "xhr anchor"
h = h.replace(old, new, 1)

# 2) apiRetry：传输类失败自动重试一次（网关忙的窗口通常几百毫秒）
anchor = "/* ---------- 网盘同步（远程挂载 → 本地，类 Cloud Sync 单向同步） ---------- */"
retry_fn = """async function apiRetry(path, opts){
  try{
    return await api(path, opts);
  }catch(e){
    // 网关忙（HTTP 0/5xx）时静默重试一次；仍失败才抛给调用方
    if(e.transport || /HTTP (0|5\\d\\d)/.test(String(e.message))){
      await new Promise(r=>setTimeout(r, 1200));
      return await api(path, opts);
    }
    throw e;
  }
}

""" + anchor
assert anchor in h, "sync anchor"
h = h.replace(anchor, retry_fn, 1)

# 3) 网盘同步的五个调用点换 apiRetry
h = h.replace("    const j = await api('/api/sync/list');", "    const j = await apiRetry('/api/sync/list');", 1)
h = h.replace("    try{ await api('/api/sync/save',{method:'POST',body:JSON.stringify(payload)});", "    try{ await apiRetry('/api/sync/save',{method:'POST',body:JSON.stringify(payload)});", 1)
h = h.replace("    try{ await api('/api/sync/delete?id='+encodeURIComponent(id),{method:'POST'});", "    try{ await apiRetry('/api/sync/delete?id='+encodeURIComponent(id),{method:'POST'});", 1)
h = h.replace("    try{ await api('/api/sync/run?id='+encodeURIComponent(id),{method:'POST'});", "    try{ await apiRetry('/api/sync/run?id='+encodeURIComponent(id),{method:'POST'});", 1)

# 4) loadSyncTasks 失败时静默（HTTP 0 由轮询下一轮自然恢复，不弹窗）
old4 = "  }catch(e){ toast(e.message,'err'); }\n}\nfunction proxyShowLog(){"
if old4 not in h:
    old4 = "  }catch(e){ toast(e.message,'err'); }\n}\nfunction proxyCopy(u){"
new4 = """  }catch(e){
    // 同步页轮询失败（多为网关忙的瞬时 HTTP 0）：静默，syncPollTick 会自动重试
    if(!/HTTP (0|5\\d\\d)/.test(String(e.message))){ toast(e.message,'err'); }
  }
}
function proxyCopy(u){"""
assert old4 in h, "catch anchor"
h = h.replace(old4, new4, 1)
io.open(p, "w", encoding="utf-8").write(h)
print("ok")
