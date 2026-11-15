function selectionUUID(){
  if(crypto.randomUUID)return crypto.randomUUID();
  const b=crypto.getRandomValues(new Uint8Array(16));
  b[6]=(b[6]&15)|64;b[8]=(b[8]&63)|128;
  return Array.from(b,(x,i)=>([4,6,8,10].includes(i)?'-':'')+x.toString(16).padStart(2,'0')).join('');
}
const libraryServer = JSON.parse(document.getElementById('library-server')?.textContent || 'null');
const libraryOffline = JSON.parse(document.getElementById('library-offline')?.textContent || 'null');

async function packRecord(record) {
  const packed = {...record};
  async function pack(blob) {
    return {name:blob.name || '', type:blob.type, data:await imageData(blob)};
  }
  for (const field of ['original','solution','file']) if (packed[field] instanceof Blob) packed[field] = await pack(packed[field]);
  if (packed.photos) packed.photos = await Promise.all(packed.photos.map(pack));
  return packed;
}
function unpackRecord(record) {
  if (!record) return record;
  // Attachments come from other teachers. Only types the page itself shows are
  // kept; anything else (text/html, SVG…) opens as a plain download, so a file
  // dressed up as a photo cannot run script on this site.
  const safeTypes = ['image/png','image/jpeg','image/gif','image/webp','application/pdf','application/vnd.openxmlformats-officedocument.wordprocessingml.document'];
  function unpack(file) {
    const bytes = Uint8Array.from(atob(file.data.slice(file.data.indexOf(',')+1)), c=>c.charCodeAt(0));
    const type = safeTypes.includes(String(file.type||'').toLowerCase()) ? file.type.toLowerCase() : 'application/octet-stream';
    return file.name ? new File([bytes],file.name,{type}) : new Blob([bytes],{type});
  }
  const result = {...record};
  for (const field of ['original','solution','file']) if (result[field]) result[field] = unpack(result[field]);
  if (result.photos) result.photos = result.photos.map(unpack);
  return result;
}
async function libraryPost(path,body) {
  const r = await fetch('library/'+path,{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify(body)});
  if (!r.ok) throw Error(await r.text() || '题库服务未能保存');
  return r;
}
// Each teacher keeps their own basket and saved papers; the rest is shared.
const personalFields=['bag','editions'];
const sharedSyncFields=['wrong','fixes','aliases','paperNotes','paperTags','purposeNames','examPurposes','deletedQuestions','deletedPapers','suggestions','purgedQuestions','purgedPapers','deletedFormats','formatAliases'];
const lastSaved={shared:null,personal:null};
const tabInstanceId=selectionUUID();
const libraryChannel=typeof BroadcastChannel!=='undefined'?new BroadcastChannel('biofolio-sync'):null;
let syncBridge=null;
function broadcastLibrarySync(payload){
  try{libraryChannel?.postMessage(JSON.parse(JSON.stringify({...payload,sender:tabInstanceId})))}catch{}
}
if(libraryChannel){
  libraryChannel.addEventListener('message',event=>{
    const msg=event.data;
    if(!msg || msg.sender===tabInstanceId || !syncBridge)return;
    syncBridge(msg);
  });
}
// Splits a state into the shared part and this teacher's part, as compared JSON.
function splitLibraryState(state){
  const shared={...state},personal={};
  for(const field of personalFields){personal[field]=state[field]||[];delete shared[field]}
  delete shared.good; // Favorites are saved per teacher through library/favorite.
  return {shared,personal,sharedText:JSON.stringify(shared),personalText:JSON.stringify(personal)};
}
// Record what the server already holds, so the first change sends only what changed.
function rememberLibraryState(state){
  if(!libraryServer)return;
  const parts=splitLibraryState(state);
  lastSaved.shared=parts.sharedText;lastSaved.personal=parts.personalText;
}
let pendingStateJob=null,stateSaveBusy=false,stateErrorHandler=null;
async function drainStateSave(){
  if(stateSaveBusy)return;
  stateSaveBusy=true;
  try{
    while(pendingStateJob){
      const job=pendingStateJob;pendingStateJob=null;
      try{
        // Shared first: it is the save that can meet another teacher's newer version.
        if(job.sharedText!==lastSaved.shared){
          const body=JSON.stringify({version:libraryServer.version,state:job.shared});
          const r=await fetch('library/state',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body,keepalive:body.length<60000});
          if(!r.ok)throw Error((await r.text()).trim()||'无法连接题库服务。');
          libraryServer.version=await r.json();
          lastSaved.shared=job.sharedText;
          broadcastLibrarySync({type:'shared',version:libraryServer.version,shared:job.shared});
        }
        if(job.personalText!==lastSaved.personal){
          const r=await fetch('library/personal',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:job.personalText,keepalive:job.personalText.length<60000});
          if(!r.ok)throw Error((await r.text()).trim()||'个人数据未能保存。');
          lastSaved.personal=job.personalText;
          broadcastLibrarySync({type:'personal',accountId:libraryServer.accountId||'',personal:job.personal});
        }
        const warning=document.getElementById('library-warning');
        if(warning)warning.textContent='';
      }catch(error){
        const later=pendingStateJob?pendingStateJob.rollbacks:[];
        pendingStateJob=null;
        for(const undo of [...later,...job.rollbacks]){try{undo()}catch{}}
        const warning=document.getElementById('library-warning');
        if(warning)warning.textContent='更改未同步：'+error.message;
        stateErrorHandler?.(error.message||'更改未同步，请刷新后重试');
      }
    }
  }finally{stateSaveBusy=false}
}
function saveLibraryState(state,undo) {
  if (!libraryServer) {localStorage.setItem(key,JSON.stringify(state));return}
  const {shared,personal,sharedText,personalText}=splitLibraryState(state);
  if(pendingStateJob){
    pendingStateJob.shared=shared;pendingStateJob.sharedText=sharedText;
    pendingStateJob.personal=personal;pendingStateJob.personalText=personalText;
    if(undo)pendingStateJob.rollbacks.unshift(undo);
  }else{
    pendingStateJob={shared,personal,sharedText,personalText,rollbacks:undo?[undo]:[]};
  }
  void drainStateSave();
}
async function serverRecords(op) {
  const read=async path=>{
    const r=await readAsset('library/'+path);
    if(!r.ok)throw Error('无法读取服务器记录，请刷新重试。');
    return r.json();
  };
  const write=async record=>{
    const response=await libraryPost('record',await packRecord(record));
    const who=await response.json();
    // Older servers answered with the username only.
    record.uploadedBy=typeof who==='string'?who:who.uploadedBy;
    record.uploaderId=typeof who==='string'?'':who.uploaderId;
    for(const q of record.questions||[]){q.uploadedBy=record.uploadedBy;q.uploaderId=record.uploaderId}
    broadcastLibrarySync({type:'record',id:record.id});
  };
  return op({getAll:()=>read('records?compact=1'),get:async id=>unpackRecord(await read('record?id='+encodeURIComponent(id))),add:write,put:write});
}
const libraryTools={
  data(){return {libraryServer,authenticatedPage:location.pathname==='/biology/',backupNotice:'',admin:false,memberName:'',memberPassword:'',memberNotice:'',accountName:'',currentPassword:'',nextPassword:'',confirmPassword:'',accountNotice:'',accountBusy:false,accountLoaded:false,accountUser:'',accountId:'',members:[],memberAdminNotice:'',resetTarget:'',resetPassword:'',resetConfirm:'',resetError:'',resetBusy:false}},
  watch:{
    page:{immediate:true,handler(page){
      this.currentPassword=this.nextPassword=this.confirmPassword='';
      this.accountNotice='';this.memberNotice='';this.memberAdminNotice='';
      if(['设置','账号管理'].includes(page))this.loadAccount();
    }}
  },
  methods:{
    // Also read once at start, so the admin-only 账号管理 entry shows in the menu.
    async loadAccount(){
      if(!libraryServer || !this.authenticatedPage)return;
      this.accountLoaded=false;
      try{
        const response=await fetch('account',{cache:'no-store'});
        if(!response.ok)throw Error(response.status===401?'登录已过期，请刷新页面。':'无法读取账号，请刷新重试。');
        const info=await response.json();this.admin=info.admin;this.members=info.members||[];this.accountName=info.username;this.accountUser=info.username;this.accountId=info.id||'';libraryServer.accountId=this.accountId;this.accountLoaded=true;
      }catch(error){this.accountNotice=error.message}
    },
    async addMember(){
      try{
        const response=await fetch('members',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify({username:this.memberName,password:this.memberPassword,confirm:this.memberPassword})});
        if(!response.ok)throw Error(await response.text());
        this.memberNotice='账号已创建：'+this.memberName;if(!this.members.includes(this.memberName))this.members=[...this.members,this.memberName];this.memberName=this.memberPassword='';
      }catch(error){this.memberNotice=error.message}
    },
    openResetMember(name){
      this.memberAdminNotice='';
      this.resetTarget=name;this.resetPassword='';this.resetConfirm='';this.resetError='';
      this.$refs.resetDialog?.showModal();
    },
    async submitResetMember(){
      if(this.resetBusy || !this.resetTarget)return;
      if(this.resetPassword.length<8){this.resetError='新密码至少需要 8 个字符。';return}
      if(this.resetPassword!==this.resetConfirm){this.resetError='两次输入的新密码不一致。';return}
      this.resetBusy=true;this.resetError='';
      try{
        const response=await fetch('member',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify({action:'reset',username:this.resetTarget,password:this.resetPassword,confirm:this.resetConfirm})});
        if(!response.ok)throw Error((await response.text()).trim()||'操作失败');
        this.memberAdminNotice='已重置 '+this.resetTarget+' 的密码';
        this.resetPassword=this.resetConfirm='';
        this.$refs.resetDialog?.close();
      }catch(error){this.resetError=error.message}finally{this.resetBusy=false}
    },
    async manageMember(name,action){
      this.memberAdminNotice='';
      if(action==='reset'){this.openResetMember(name);return}
      if(!confirm('删除教师账号 '+name+'？该教师将立即退出，此操作不可撤销。'))return;
      try{
        const response=await fetch('member',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify({action,username:name,password:'',confirm:''})});
        if(!response.ok)throw Error((await response.text()).trim()||'操作失败');
        this.memberAdminNotice='已删除 '+name;
        this.members=this.members.filter(n=>n!==name);
      }catch(error){this.memberAdminNotice=error.message}
    },
    async updateAccount(){
      if(this.accountBusy || !this.accountLoaded)return;
      this.accountNotice='';
      if(this.nextPassword && this.nextPassword!==this.confirmPassword){this.accountNotice='两次输入的新密码不一致';return}
      this.accountBusy=true;
      try{
        const response=await fetch('account',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify({username:this.accountName,current:this.currentPassword,password:this.nextPassword,confirm:this.nextPassword?this.confirmPassword:''})});
        if(!response.ok)throw Error((await response.text()).trim()||'账号保存失败');
        this.currentPassword=this.nextPassword=this.confirmPassword='';
        location.reload();
      }catch(error){this.accountNotice=error.message}finally{this.accountBusy=false}
    },
    async backupLibrary(){
      try{
        const records=await accessRecords('readonly',s=>s.getAll());
        const state=JSON.parse(localStorage.getItem(key)||'null') || libraryOffline?.state || {};
        const blob=new Blob([JSON.stringify({state,seed:dataset,records:await Promise.all(records.map(packRecord))})],{type:'application/json'});
        const url=URL.createObjectURL(blob),a=document.createElement('a');a.href=url;a.download='个人题库备份.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),30000);
      }catch(error){this.backupNotice=error.message}
    },
    async restoreLibrary(event){
      const file=event.target.files[0];event.target.value='';if(!file)return;
      try{
        const data=JSON.parse(await file.text());if(!data.state||!Array.isArray(data.records))throw Error('请选用题库导出的备份。');
        await libraryPost('restore',data);location.reload();
      }catch(error){this.backupNotice=error.message}
    }
  }
};

// Offline pages already contain the libraries. On the NAS, load each tool
// on its first use, sharing one request across concurrent operations.
const toolRequests=new Map();
function loadTool(name){
  const url=document.getElementById('lib-'+name)?.dataset.url;
  if(!url)return Promise.resolve();
  if(!toolRequests.has(name)){
    const request=new Promise((resolve,reject)=>{
      const script=document.createElement('script');script.src=url;
      script.onload=resolve;script.onerror=()=>reject(Error('组件加载失败，请检查连接后重试'));
      document.head.append(script);
    }).catch(error=>{toolRequests.delete(name);throw error});
    toolRequests.set(name,request);
  }
  return toolRequests.get(name);
}
async function readAsset(url){
  const metadata=url.includes('seed-questions')||url.includes('library/records');
  const response=await fetch(url,metadata?{signal:AbortSignal.timeout(20000)}:{});if(!response.ok)throw Error(response.status===401?'登录已失效，请重新登录。':'内容加载失败，请重试。');
  return response;
}

function libraryPicture(src){return typeof src==='string' && /^library\/picture\/[a-f0-9]{64}$/.test(src)}
async function inlinePicture(src){
  return libraryPicture(src)?imageData(await (await readAsset(src)).blob()):src;
}
const pictureKeyCache=new Map();
async function pictureKey(src){
  if(!libraryServer || libraryPicture(src))return src;
  if(pictureKeyCache.has(src))return pictureKeyCache.get(src);
  const hashed='library/picture/'+await fileDigest(new TextEncoder().encode(src));
  if(pictureKeyCache.size>2000)pictureKeyCache.clear();
  pictureKeyCache.set(src,hashed);
  return hashed;
}
