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
  function unpack(file) {
    const bytes = Uint8Array.from(atob(file.data.slice(file.data.indexOf(',')+1)), c=>c.charCodeAt(0));
    return file.name ? new File([bytes],file.name,{type:file.type}) : new Blob([bytes],{type:file.type});
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
const lastSaved={shared:null,personal:null};
function postStateSync(path,body){
  const request = new XMLHttpRequest();
  request.open('POST',path,false);
  request.setRequestHeader('Content-Type','application/json');
  request.setRequestHeader('X-Biology-Library','1');
  request.send(body);
  if (request.status<200 || request.status>=300) throw Error(request.responseText || '无法连接题库服务。');
  return request.responseText;
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
function saveLibraryState(state) {
  if (!libraryServer) {localStorage.setItem(key,JSON.stringify(state));return}
  // Existing save actions are synchronous. Only small controls/metadata use these endpoints;
  // originals, images and versioned question content use the async records store.
  const {shared,sharedText,personalText}=splitLibraryState(state);
  try {
    // Shared first: it is the save that can meet another teacher's newer version.
    if (sharedText!==lastSaved.shared) {
      libraryServer.version=JSON.parse(postStateSync('library/state',JSON.stringify({version:libraryServer.version,state:shared})));
      lastSaved.shared=sharedText;
    }
    if (personalText!==lastSaved.personal) {
      postStateSync('library/personal',personalText);
      lastSaved.personal=personalText;
    }
  } catch(error) {
    const warning=document.getElementById('library-warning');
    if(warning)warning.textContent='更改未同步。'+error.message;
    throw error;
  }
  // A later successful save means the earlier failure no longer applies.
  const warning=document.getElementById('library-warning');
  if(warning)warning.textContent='';
}
async function serverRecords(op) {
  const read=async path=>{
    const r=await readAsset('library/'+path);
    if(!r.ok)throw Error('无法读取服务器记录，请刷新重试。');
    return r.json();
  };
  const write=async record=>{
    const response=await libraryPost('record',await packRecord(record));
    record.uploadedBy=await response.json();
    for(const q of record.questions||[])q.uploadedBy=record.uploadedBy;
  };
  return op({getAll:()=>read('records?compact=1'),get:async id=>unpackRecord(await read('record?id='+encodeURIComponent(id))),add:write,put:write});
}
const libraryTools={
  data(){return {libraryServer,authenticatedPage:location.pathname==='/biology/',backupNotice:'',admin:false,memberName:'',memberPassword:'',memberNotice:'',accountName:'',currentPassword:'',nextPassword:'',confirmPassword:'',accountNotice:'',accountBusy:false,accountLoaded:false,members:[],memberAdminNotice:''}},
  watch:{
    page:{immediate:true,async handler(page){
      this.currentPassword=this.nextPassword=this.confirmPassword='';
      this.accountNotice='';this.accountLoaded=false;
      if(page!=='设置' || !libraryServer || !this.authenticatedPage)return;
      try{
        const response=await fetch('account',{cache:'no-store'});
        if(!response.ok)throw Error(response.status===401?'登录已过期，请刷新页面。':'无法读取账号，请刷新重试。');
        const info=await response.json();this.admin=info.admin;this.members=info.members||[];this.accountName=info.username;this.accountLoaded=true;
      }catch(error){this.accountNotice=error.message}
    }}
  },
  methods:{
    async addMember(){
      try{
        const response=await fetch('members',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify({username:this.memberName,password:this.memberPassword,confirm:this.memberPassword})});
        if(!response.ok)throw Error(await response.text());
        this.memberNotice='账号已创建：'+this.memberName;this.memberName=this.memberPassword='';
      }catch(error){this.memberNotice=error.message}
    },
    async manageMember(name,action){
      this.memberAdminNotice='';
      let password='';
      if(action==='reset'){
        password=prompt('为教师 '+name+' 设置新密码（至少 8 个字符）')||'';
        if(!password)return;
      }else if(!confirm('删除教师账号 '+name+'？该教师将立即退出，此操作不可撤销。'))return;
      try{
        const response=await fetch('member',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify({action,username:name,password,confirm:password})});
        if(!response.ok)throw Error((await response.text()).trim()||'操作失败');
        this.memberAdminNotice=action==='reset'?'已重置 '+name+' 的密码':'已删除 '+name;
        if(action==='delete')this.members=this.members.filter(n=>n!==name);
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
async function pictureKey(src){
  if(!libraryServer || libraryPicture(src))return src;
  return 'library/picture/'+await fileDigest(new TextEncoder().encode(src));
}
