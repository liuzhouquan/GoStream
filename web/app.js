const state = { token: localStorage.getItem('gostream.token') || '', user: JSON.parse(localStorage.getItem('gostream.user') || 'null'), view: 'feed', scene: 'recommend', cursor: '', loading: false, socket: null, chatPeer: '', seenMessages: new Set(), groupID: '', groupSeenMessages: new Set(), groups: [], socketRetry: 0, socketTimer: null, heartbeatTimer: null };
const $ = (selector, root = document) => root.querySelector(selector);
const content = $('#content');
const titles = {feed:'发现灵感',following:'关注动态',hot:'热门视频',search:'发现创作者',upload:'发布作品',notifications:'通知中心',chat:'即时消息',profile:'个人主页'};
const esc = (s='') => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
async function api(path, options={}) {
  const headers = new Headers(options.headers || {});
  if (state.token) headers.set('Authorization', `Bearer ${state.token}`);
  if (options.body && !(options.body instanceof FormData)) headers.set('Content-Type','application/json');
  const res = await fetch(path,{...options,headers});
  if (res.status === 204) return null;
  const data = await res.json().catch(()=>({}));
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data;
}
function toast(text) { const el=$('#toast');el.textContent=text;el.classList.add('show');setTimeout(()=>el.classList.remove('show'),2400); }
function setAuth(token,user) { state.token=token;state.user=user;localStorage.setItem('gostream.token',token);localStorage.setItem('gostream.user',JSON.stringify(user));renderSession();ensureSocket();$('#auth-dialog').close();toast(`欢迎来到 GoStream，${user.nickname}`);loadView(); }
function clearAuth() { if(state.socketTimer)clearTimeout(state.socketTimer);if(state.heartbeatTimer)clearInterval(state.heartbeatTimer);state.socketTimer=null;state.heartbeatTimer=null;if(state.socket)state.socket.close();state.socket=null;state.groupID='';state.chatPeer='';state.token='';state.user=null;localStorage.removeItem('gostream.token');localStorage.removeItem('gostream.user');renderSession();loadView(); }
function renderSession() { $('#session-state').textContent=state.user?`你好，${state.user.nickname}`:'游客浏览中';$('#auth-open').textContent=state.user?'退出登录':'登录 / 注册';$('#top-avatar').innerHTML=state.user?.avatar_url?`<img src="${esc(state.user.avatar_url)}" alt="头像" />`:esc(state.user?(state.user.nickname||'G').slice(0,1).toUpperCase():'G'); }
function activate(view) { state.view=view;state.cursor='';document.querySelectorAll('.nav-item').forEach(x=>x.classList.toggle('active',x.dataset.view===view));$('#page-title').textContent=titles[view]||titles.feed;document.querySelectorAll('.scene').forEach(x=>x.classList.toggle('active',x.dataset.scene===state.scene));loadView(); }
function formatCount(n=0) { if(n>=10000)return `${(n/10000).toFixed(1)}万`;if(n>=1000)return `${(n/1000).toFixed(1)}k`;return String(n); }
function formatDuration(ms=0) { const seconds=Math.round(Number(ms)/1000);if(!Number.isFinite(seconds)||seconds<1)return '';return `${Math.floor(seconds/60)}:${String(seconds%60).padStart(2,'0')}`; }
function processingFailureText(code) { return ({duration_limit:'视频超过 10 分钟',resolution_limit:'视频画面尺寸超过限制',invalid_duration:'无法读取视频时长',missing_video:'文件中没有可用视频画面',invalid_media:'视频文件无法解析'})[code]||'处理失败，请重新上传'; }
function videoCard(v) {
  const ready=v.status==='published';
  const media=esc(v.media_url||'');
  const failure=v.status==='failed'?processingFailureText(v.processing_error):'';
  const body=ready&&/\.(mp4|webm|mov)(\?|$)/i.test(media)?`<video src="${media}" muted playsinline preload="metadata" controls${v.cover_url?` poster="${esc(v.cover_url)}"`:''}></video>`:`<div class="poster-fallback"><span>✳</span><small>${v.status==='failed'?failure:ready?'GOSTREAM ORIGINAL':'视频处理中'}</small></div>`;
  const actions=ready?`<div class="card-actions"><button class="icon-action ${v.liked?'on':''}" data-action="like" data-id="${v.id}" title="点赞">${v.liked?'♥':'♡'} ${formatCount(v.like_count)}</button><button class="icon-action ${v.favorited?'on':''}" data-action="favorite" data-id="${v.id}" title="收藏">${v.favorited?'★':'☆'} ${formatCount(v.favorite_count)}</button><button class="icon-action" data-action="comment" data-id="${v.id}" title="评论">◌ ${formatCount(v.comment_count)}</button></div>`:'';
  return `<article class="video-card" data-video="${v.id}"><div class="poster">${body}${ready?`<button class="play-overlay" data-play="${v.id}">▶</button>`:''}<div class="poster-caption"><span class="category">${esc(v.author?.nickname||'CREATOR')}</span><h3>${esc(v.title)}</h3></div></div><div class="card-footer"><div class="creator"><span class="mini-avatar">${esc((v.author?.nickname||'G').slice(0,1).toUpperCase())}</span><div class="creator-info"><span class="creator-name">${esc(v.author?.nickname||'创作者')}</span><span class="creator-meta">${ready?`▶ ${formatCount(v.play_count)} 次播放${v.duration_ms?` · ${formatDuration(v.duration_ms)}`:''}`:v.status==='failed'?failure:'处理中'}</span></div></div>${actions}</div><div class="comment-slot" id="comments-${v.id}"></div></article>`;
}
async function loadFeed(scene=state.scene) { const append=Boolean(state.cursor);const previous=append?[...content.children].filter(x=>!x.matches('.load-more')).map(x=>x.outerHTML).join(''):'';state.scene=scene;document.querySelectorAll('.scene').forEach(x=>x.classList.toggle('active',x.dataset.scene===scene));const data=await api(`/api/feed?scene=${encodeURIComponent(scene)}&limit=18${state.cursor?`&cursor=${encodeURIComponent(state.cursor)}`:''}`);state.cursor=data.next_cursor||'';const cards=(data.items||[]).map(videoCard).join('');content.innerHTML=previous+(cards||(!append?'<div class="empty-state"><div class="dialog-kicker">YOUR NEXT FAVORITE</div><h2>这里还没有视频</h2><p>发布一段视频，开始构建你的社区。</p></div>':''));if(data.has_more)content.insertAdjacentHTML('beforeend','<button class="primary-button load-more">加载更多</button>'); }
async function loadView() { if(state.loading)return;state.loading=true;content.innerHTML='<div class="empty-state">正在加载…</div>';try { if(['feed','following','hot'].includes(state.view)){const scene=state.view==='feed'?state.scene:state.view;await loadFeed(scene);}else if(state.view==='upload')renderUpload();else if(state.view==='notifications')await renderNotifications();else if(state.view==='chat')await renderChat();else if(state.view==='profile')await renderProfile();else if(state.view==='search')renderSearch();}catch(e){content.innerHTML=`<div class="empty-state"><h2>暂时无法加载</h2><p>${esc(e.message)}</p>${!state.token?'<button class="primary-button" data-auth="open">登录 GoStream</button>':''}</div>`;}finally{state.loading=false;} }
function renderUpload(){if(!state.token){content.innerHTML='<div class="empty-state"><h2>登录后发布作品</h2><button class="primary-button" data-auth="open">登录 / 注册</button></div>';return;}content.innerHTML=`<section class="panel"><span class="pill">CREATOR STUDIO · 01</span><h2>发布一段好内容</h2><p>选择视频并填写简介。上传支持分片和断点续传；启用视频处理时，视频最长 10 分钟、画面总像素不超过 3840×2160，转码完成后作品会加入公开 Feed。</p><form id="upload-form" class="form-grid"><label>视频文件<input name="file" type="file" accept="video/mp4,video/webm,video/quicktime" required /></label><label>作品标题<input name="title" maxlength="120" placeholder="给这段内容起个标题" required /></label><label>简介<textarea name="description" maxlength="2000" placeholder="说说视频背后的故事"></textarea></label><button class="primary-button">上传并发布 →</button><p id="upload-progress" role="status"></p></form></section>`;}
async function uploadResumable(file) {
  if(file.size<1 || file.size>250*1024*1024) throw new Error('视频大小必须在 1 字节至 250 MiB 之间');
  const key=`gostream.upload.${state.user.id}.${file.name}.${file.size}.${file.lastModified}`;
  const progress=$('#upload-progress');
  let session;
  const existingID=localStorage.getItem(key);
  if(existingID){
    try { session=await api(`/api/uploads/sessions/${existingID}`); }
    catch (_) { localStorage.removeItem(key); }
  }
  if(session?.status==='completed') return session.url;
  if(!session){
    session=await api('/api/uploads/sessions',{method:'POST',body:JSON.stringify({filename:file.name,size:file.size})});
    localStorage.setItem(key,session.id);
  }
  if(session.status&&session.status!=='open') throw new Error('上传任务正在处理，请稍后重新选择文件继续');
  const chunks=Math.ceil(file.size/session.chunk_size);
  const uploaded=new Set(session.uploaded_chunks||[]);
  for(let index=0;index<chunks;index++){
    if(uploaded.has(index)) continue;
    const part=file.slice(index*session.chunk_size,Math.min(file.size,(index+1)*session.chunk_size));
    const response=await fetch(`/api/uploads/sessions/${session.id}/chunks/${index}`,{
      method:'PUT',headers:{Authorization:`Bearer ${state.token}`,'Content-Type':'application/octet-stream'},body:part
    });
    const result=await response.json().catch(()=>({}));
    if(!response.ok) throw new Error(result.error||`第 ${index+1} 片上传失败`);
    uploaded.add(index);
    if(progress) progress.textContent=`已上传 ${uploaded.size}/${chunks} 片`;
  }
  if(progress) progress.textContent='正在合并并校验视频…';
  const complete=await api(`/api/uploads/sessions/${session.id}/complete`,{method:'POST'});
  return complete.url;
}
async function uploadAvatar(file) {
  const body=new FormData();body.append('file',file);
  const response=await fetch('/api/uploads',{method:'POST',headers:{Authorization:`Bearer ${state.token}`},body});
  const result=await response.json().catch(()=>({}));
  if(!response.ok)throw new Error(result.error||'头像上传失败');
  return result.url;
}
async function renderProfile(){
  if(!state.user){content.innerHTML='<div class="empty-state"><h2>登录后查看个人主页</h2><button class="primary-button" data-auth="open">登录 / 注册</button></div>';return;}
  const [me,videos]=await Promise.all([api('/api/users/me'),api(`/api/users/${state.user.id}/videos`)]);
  state.user=me;localStorage.setItem('gostream.user',JSON.stringify(me));renderSession();
  const avatar=me.avatar_url?`<img class="profile-avatar" src="${esc(me.avatar_url)}" alt="${esc(me.nickname)}的头像" />`:'';
  const cards=(videos.items||[]).map(v=>`<div class="managed-video">${videoCard(v)}<div class="manage-actions"><button class="text-button" data-edit-video="${v.id}">编辑</button><button class="text-button" data-delete-video="${v.id}">删除</button></div><div id="edit-video-${v.id}"></div></div>`).join('');
  content.innerHTML=`<section class="panel"><span class="pill">YOUR CREATOR SPACE</span>${avatar}<h2>${esc(me.nickname)} <small>@${esc(me.username)}</small></h2><p>${esc(me.bio||'还没有个人介绍。')}</p><form id="profile-form" class="form-grid"><label>昵称<input name="nickname" value="${esc(me.nickname)}" maxlength="48" required /></label><label>个人简介<textarea name="bio" maxlength="280">${esc(me.bio||'')}</textarea></label><label>头像图片<input name="avatar" type="file" accept="image/png,image/jpeg,image/webp" /></label><button class="primary-button">保存资料</button></form><h2 style="margin-top:30px">我的作品</h2><div class="content-grid">${cards||'<p>还没有发布作品。</p>'}</div></section>`;
}
function renderSearch(){content.innerHTML='<section class="panel"><span class="pill">SEARCH · EXPLORE</span><h2>找视频，也找创作者</h2><p>搜索至少两个字符，看看社区里正在发生什么。</p><form id="search-form" class="form-grid"><label>关键词<input class="search-large" name="q" minlength="2" maxlength="80" placeholder="例如：摄影、旅行、料理" required /></label><button class="primary-button">开始搜索</button></form><div id="search-results" class="message-list"></div></section>';}
async function renderNotifications(){if(!state.token){content.innerHTML='<div class="empty-state"><h2>登录查看通知</h2><button class="primary-button" data-auth="open">登录 / 注册</button></div>';return;}const data=await api('/api/notifications');content.innerHTML=`<section class="panel"><span class="pill">ACTIVITY</span><h2>最近的互动</h2><div class="message-list">${(data.items||[]).map(n=>`<div class="message-row"><b>${esc(n.actor?.nickname||'GoStream')}</b> ${esc(n.text||n.kind)} <small>${new Date(n.created_at).toLocaleString()}</small></div>`).join('')||'<p>暂时没有新通知。</p>'}</div><button class="text-button" data-read-all>全部标记为已读</button></section>`;}
async function renderChat(){
  if(!state.token){content.innerHTML='<div class="empty-state"><h2>登录后开始聊天</h2><button class="primary-button" data-auth="open">登录 / 注册</button></div>';return;}
  const groups=await api('/api/groups');
  state.groups=groups.items||[];
  content.innerHTML=`<section class="panel"><span class="pill">REALTIME · WEBSOCKET</span><h2>即时消息</h2><p>私信和群消息会先保存，再尝试实时投递。离线消息可从历史记录读取。</p><h3>单聊</h3><div class="form-grid"><label>聊天对象用户 ID<input id="chat-peer" type="number" min="1" placeholder="对方的用户 ID" value="${esc(state.chatPeer)}" /></label><button class="primary-button" data-chat-connect>加载聊天历史</button></div><p id="chat-presence" role="status"></p><div id="chat-log" class="message-list"></div><form id="chat-form" class="form-grid"><label>发送私信<textarea name="body" maxlength="4000" required></textarea></label><button class="primary-button">发送 →</button></form><h3 class="chat-section-title">群聊</h3><form id="group-create-form" class="form-grid"><label>新群名称<input name="name" maxlength="80" placeholder="例如 Go 后端学习群" required /></label><button class="primary-button">创建群聊</button></form><div id="group-list" class="group-list">${(groups.items||[]).map(g=>`<button class="text-button group-chip" data-group-open="${g.id}">${esc(g.name)} · #${g.id}</button>`).join('')||'<p>还没有加入群聊。</p>'}</div><div id="group-panel"></div></section>`;
  ensureSocket();
  if(state.chatPeer)connectChat();
  if(state.groupID)openGroup(state.groupID).catch(err=>toast(err.message));
}
function ensureSocket(){
  if(!state.token || (state.socket&&state.socket.readyState<=1))return;
  const protocol=location.protocol==='https:'?'wss':'ws';
  const ws=new WebSocket(`${protocol}://${location.host}/api/ws`,['gostream',`gostream.jwt.${state.token}`]);
  state.socket=ws;
  ws.onopen=()=>{state.socketRetry=0;if(state.heartbeatTimer)clearInterval(state.heartbeatTimer);state.heartbeatTimer=setInterval(()=>{if(ws.readyState===1)ws.send(JSON.stringify({type:'ping'}));},25000);toast('即时消息已连接');};
  ws.onmessage=event=>{
    const data=JSON.parse(event.data);
    if(data.type==='message'){
      if(String(data.message.sender_id)===String(state.chatPeer) && $('#chat-log')){
        appendMessage(data.message);
        api(`/api/messages/${data.message.id}/ack`,{method:'POST'}).catch(()=>{});
      }else toast('收到一条新私信');
    }else if(data.type==='group_message'){
      if(String(data.message.group_id)===String(state.groupID) && $('#group-log')){
        appendGroupMessage(data.message);
        api(`/api/groups/${data.message.group_id}/messages/${data.message.id}/ack`,{method:'POST'}).catch(()=>{});
      }else toast('收到一条新群消息');
    }else if(data.type==='notification'){
      toast('收到一条新通知');if(state.view==='notifications')renderNotifications();
    }else if(data.type==='sync_required'||data.type==='group_sync_required')toast('离线消息较多，请打开聊天历史查看');
    else if(data.type==='error')toast(data.message);
  };
  ws.onclose=()=>{
    if(state.heartbeatTimer)clearInterval(state.heartbeatTimer);state.heartbeatTimer=null;
    if(state.socket!==ws)return;
    state.socket=null;
    if(state.token){const delay=Math.min(30000,1000*2**Math.min(state.socketRetry++,5));state.socketTimer=setTimeout(ensureSocket,delay);}
  };
}
async function connectChat(){
  if(!state.token)return;
  state.chatPeer=$('#chat-peer')?.value||state.chatPeer;
  if(!state.chatPeer)return;
  const presence=await api(`/api/users/${encodeURIComponent(state.chatPeer)}/presence`);
  const status=$('#chat-presence');if(status)status.textContent=presence.online?'对方在线':presence.last_seen_at?`最近在线：${new Date(presence.last_seen_at).toLocaleString()}`:'对方离线';
  const history=await api(`/api/messages?user_id=${encodeURIComponent(state.chatPeer)}`);
  const log=$('#chat-log');
  if(log){
    state.seenMessages.clear();
    log.innerHTML=(history.items||[]).reverse().map(m=>`<div class="message-row"><b>${m.sender_id===state.user.id?'你':'对方'}</b> ${esc(m.body)}<small>${new Date(m.created_at).toLocaleString()}</small></div>`).join('');
    (history.items||[]).forEach(m=>state.seenMessages.add(m.id));
  }
  ensureSocket();
}
function appendMessage(m){
  if(m.id&&state.seenMessages.has(m.id))return;
  if(m.id)state.seenMessages.add(m.id);
  const log=$('#chat-log');
  if(log){log.insertAdjacentHTML('beforeend',`<div class="message-row"><b>${m.sender_id===state.user.id?'你':'对方'}</b> ${esc(m.body)}<small>${new Date(m.created_at).toLocaleTimeString()}</small></div>`);log.scrollTop=log.scrollHeight;}
}
async function openGroup(id){
  state.groupID=String(id);
  const history=await api(`/api/groups/${id}/messages?limit=100`);
  const panel=$('#group-panel');if(!panel)return;
  state.groupSeenMessages.clear();
  const items=(history.items||[]).reverse();
  const group=state.groups.find(g=>String(g.id)===String(id));
  const controls=group?.owner_id===state.user.id?`<form id="group-add-form" class="form-grid"><label>邀请用户 ID<input name="user_id" type="number" min="1" required /></label><button class="text-button">添加成员</button></form><form id="group-remove-form" class="form-grid"><label>移除成员 ID<input name="user_id" type="number" min="1" required /></label><button class="text-button">移除成员</button></form>`:`<button class="text-button group-leave" data-group-leave="${esc(id)}">退出群聊</button>`;
  panel.innerHTML=`<h3>群聊 #${esc(id)}</h3><div id="group-log" class="message-list">${items.map(m=>`<div class="message-row"><b>${m.sender_id===state.user.id?'你':`用户 ${m.sender_id}`}</b> ${esc(m.body)}<small>${new Date(m.created_at).toLocaleString()}</small></div>`).join('')}</div><form id="group-send-form" class="form-grid"><label>发送群消息<textarea name="body" maxlength="4000" required></textarea></label><button class="primary-button">发送到群聊 →</button></form>${controls}`;
  items.forEach(m=>state.groupSeenMessages.add(m.id));
  if(items.length)api(`/api/groups/${id}/messages/${items[items.length-1].id}/ack`,{method:'POST'}).catch(()=>{});
  ensureSocket();
}
function appendGroupMessage(m){
  if(m.id&&state.groupSeenMessages.has(m.id))return;
  if(m.id)state.groupSeenMessages.add(m.id);
  const log=$('#group-log');
  if(log){log.insertAdjacentHTML('beforeend',`<div class="message-row"><b>${m.sender_id===state.user.id?'你':`用户 ${m.sender_id}`}</b> ${esc(m.body)}<small>${new Date(m.created_at).toLocaleTimeString()}</small></div>`);log.scrollTop=log.scrollHeight;}
}
document.addEventListener('click',async e=>{const nav=e.target.closest('[data-view]');if(nav){activate(nav.dataset.view);return;}const scene=e.target.closest('[data-scene]');if(scene){state.view='feed';state.cursor='';$('#page-title').textContent=titles.feed;await loadFeed(scene.dataset.scene);return;}if(e.target.closest('[data-auth="open"]')||e.target.id==='auth-open'&&!state.user){$('#auth-dialog').showModal();return;}if(e.target.id==='auth-open'&&state.user){clearAuth();toast('已退出登录');return;}const edit=e.target.closest('[data-edit-video]');if(edit){try{const v=await api(`/api/videos/${edit.dataset.editVideo}`);const slot=$(`#edit-video-${v.id}`);slot.innerHTML=`<form class="form-grid video-edit-form" data-video-id="${v.id}"><label>标题<input name="title" maxlength="120" value="${esc(v.title)}" required /></label><label>简介<textarea name="description" maxlength="2000">${esc(v.description||'')}</textarea></label><button class="primary-button">保存视频</button></form>`;}catch(err){toast(err.message);}return;}
const remove=e.target.closest('[data-delete-video]');if(remove){if(!confirm('确定删除这个视频吗？'))return;try{await api(`/api/videos/${remove.dataset.deleteVideo}`,{method:'DELETE'});toast('视频已删除');renderProfile();}catch(err){toast(err.message);}return;}
const act=e.target.closest('[data-action]');if(act){if(!state.token){$('#auth-dialog').showModal();return;}try{if(act.dataset.action==='like'||act.dataset.action==='favorite'){await api(`/api/videos/${act.dataset.id}/${act.dataset.action}`,{method:act.classList.contains('on')?'DELETE':'PUT'});act.classList.toggle('on');const n=Number((act.textContent.match(/\d+/)||[0])[0]);const active=act.classList.contains('on');const icon=act.dataset.action==='like'?(active?'♥':'♡'):(active?'★':'☆');act.innerHTML=`${icon} ${formatCount(n+(active?1:-1))}`;}else{const slot=$(`#comments-${act.dataset.id}`);if(slot.innerHTML){slot.innerHTML='';return;}slot.innerHTML='正在加载评论…';const data=await api(`/api/videos/${act.dataset.id}/comments`);slot.innerHTML=`${(data.items||[]).map(c=>`<div class="message-row"><b>${esc(c.user?.nickname||'用户')}</b> ${esc(c.body)}</div>`).join('')}<form class="comment-form" data-comment-video="${act.dataset.id}"><input name="body" maxlength="2000" placeholder="写下你的评论" required /><button>发送</button></form>`;}}catch(err){toast(err.message);}return;}if(e.target.closest('.load-more')){state.cursor=state.cursor||'';loadFeed(state.scene);return;}const follow=e.target.closest('[data-follow]');if(follow){if(!state.token){$('#auth-dialog').showModal();return;}try{await api(`/api/users/me/following/${follow.dataset.follow}`,{method:'PUT'});follow.textContent='已关注';follow.disabled=true;}catch(err){toast(err.message);}return;}if(e.target.closest('[data-read-all]')){await api('/api/notifications/read',{method:'PATCH'});toast('通知已标记为已读');renderNotifications();return;}if(e.target.closest('[data-chat-connect]')){state.chatPeer=$('#chat-peer').value;connectChat().catch(err=>toast(err.message));return;}const open=e.target.closest('[data-group-open]');if(open){openGroup(open.dataset.groupOpen).catch(err=>toast(err.message));return;}const leave=e.target.closest('[data-group-leave]');if(leave){try{await api(`/api/groups/${leave.dataset.groupLeave}/members/${state.user.id}`,{method:'DELETE'});state.groupID='';await renderChat();toast('已退出群聊');}catch(err){toast(err.message);}return;}if(e.target.closest('[data-play]')){const v=e.target.closest('.poster').querySelector('video');if(v){v.paused?v.play():v.pause();api(`/api/videos/${e.target.closest('[data-play]').dataset.play}/views`,{method:'POST'}).catch(()=>{});}return;}});
document.addEventListener('submit',async e=>{e.preventDefault();const form=e.target;try{if(form.id==='auth-form'){const fd=new FormData(form),register=$('#auth-submit').dataset.mode==='register';const data=await api(register?'/api/users':'/api/sessions',{method:'POST',body:JSON.stringify({username:fd.get('username'),password:fd.get('password'),nickname:fd.get('nickname')})});setAuth(data.access_token,data.user);}else if(form.id==='upload-form'){const fd=new FormData(form),file=fd.get('file');const url=await uploadResumable(file);const published=await api('/api/videos',{method:'POST',body:JSON.stringify({title:fd.get('title'),description:fd.get('description'),media_url:url})});localStorage.removeItem(`gostream.upload.${state.user.id}.${file.name}.${file.size}.${file.lastModified}`);toast(published.status==='processing'?'视频处理中，请在个人主页查看进度':'作品已发布');activate(published.status==='processing'?'profile':'feed');}else if(form.id==='profile-form'){const fd=new FormData(form);const fields={nickname:fd.get('nickname'),bio:fd.get('bio')};const avatar=fd.get('avatar');if(avatar?.size)fields.avatar_url=await uploadAvatar(avatar);state.user=await api('/api/users/me',{method:'PATCH',body:JSON.stringify(fields)});localStorage.setItem('gostream.user',JSON.stringify(state.user));toast('个人资料已保存');renderProfile();}else if(form.classList.contains('video-edit-form')){const fd=new FormData(form);await api(`/api/videos/${form.dataset.videoId}`,{method:'PATCH',body:JSON.stringify({title:fd.get('title'),description:fd.get('description')})});toast('视频资料已更新');renderProfile();}else if(form.id==='search-form'){const q=new FormData(form).get('q');const data=await api(`/api/search?q=${encodeURIComponent(q)}`);$('#search-results').innerHTML=[...(data.users||[]).map(u=>`<div class="message-row"><b>${esc(u.nickname)}</b> <span class="creator-meta">@${esc(u.username)} · 用户 ${u.id}</span><button class="text-button" data-follow="${u.id}">关注</button></div>`),...(data.videos||[]).map(v=>`<div class="message-row"><b>${esc(v.title)}</b><small>${esc(v.author?.nickname||'')}</small></div>`)].join('')||'<p>没有找到结果。</p>';}else if(form.classList.contains('comment-form')){await api(`/api/videos/${form.dataset.commentVideo}/comments`,{method:'POST',body:JSON.stringify({body:new FormData(form).get('body')})});form.reset();toast('评论已发布');}else if(form.id==='group-create-form'){const group=await api('/api/groups',{method:'POST',body:JSON.stringify({name:new FormData(form).get('name')})});state.groupID=String(group.id);await renderChat();toast('群聊已创建');}else if(form.id==='group-add-form'){await api(`/api/groups/${state.groupID}/members`,{method:'POST',body:JSON.stringify({user_id:Number(new FormData(form).get('user_id'))})});form.reset();toast('成员已添加');}else if(form.id==='group-remove-form'){const userID=Number(new FormData(form).get('user_id'));await api(`/api/groups/${state.groupID}/members/${userID}`,{method:'DELETE'});form.reset();toast('成员已移除');}else if(form.id==='group-send-form'){const body=new FormData(form).get('body');if(!state.socket||state.socket.readyState!==1)throw new Error('群聊连接中，请稍后重试');state.socket.send(JSON.stringify({type:'group_message',client_id:crypto.randomUUID(),group_id:Number(state.groupID),body}));appendGroupMessage({sender_id:state.user.id,body,created_at:new Date().toISOString()});form.reset();}else if(form.id==='chat-form'){const body=new FormData(form).get('body');if(!state.socket||state.socket.readyState!==1)throw new Error('请先连接聊天');const client_id=crypto.randomUUID();state.socket.send(JSON.stringify({type:'message',client_id,recipient_id:Number(state.chatPeer),body}));appendMessage({sender_id:state.user.id,body,created_at:new Date().toISOString()});form.reset();}}catch(err){toast(err.message);}});
$('#auth-form').addEventListener('submit',()=>{});
$('#auth-switch').addEventListener('click',()=>{const register=$('#auth-submit').dataset.mode!=='register';$('#auth-submit').dataset.mode=register?'register':'login';$('#auth-submit').textContent=register?'创建账号':'登录';$('#auth-title').textContent=register?'创建你的创作者账号':'欢迎回来';$('#nickname-wrap').hidden=!register;$('#auth-switch').textContent=register?'已有账号？登录':'还没有账号？立即注册';$('#auth-form input[name=password]').autocomplete=register?'new-password':'current-password';});
$('#auth-dialog .dialog-close button').addEventListener('click',()=>$('#auth-dialog').close());
$('#top-avatar').addEventListener('click',()=>state.user?activate('profile'):$('#auth-dialog').showModal());
$('#global-search').addEventListener('keydown',e=>{if(e.key==='Enter'){activate('search');setTimeout(()=>{const input=$('#search-form input[name=q]');if(input){input.value=e.target.value;$('#search-form').requestSubmit();}},0);}});
document.addEventListener('change',e=>{if(e.target.id==='chat-peer')state.chatPeer=e.target.value;});
renderSession();if(state.token)ensureSocket();activate('feed');
