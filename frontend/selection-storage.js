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
}
function saveLibraryState(state) {
  if (!libraryServer) {localStorage.setItem(key,JSON.stringify(state));return}
  // Existing save actions are synchronous. Only small controls/metadata use this endpoint;
  // originals, images and versioned question content use the async records store.
  const request = new XMLHttpRequest();
  request.open('POST','library/state',false);
  request.setRequestHeader('Content-Type','application/json');
  request.setRequestHeader('X-Biology-Library','1');
  try {
    request.send(JSON.stringify({version:libraryServer.version,state}));
    if (request.status!==200) throw Error(request.responseText || '无法连接题库服务。');
    libraryServer.version=JSON.parse(request.responseText);
  } catch(error) {
    document.getElementById('library-warning').textContent='更改未同步。'+error.message;
    throw error;
  }
}
async function serverRecords(op) {
  const read=async path=>{
    const r=await fetch('library/'+path);
    if(!r.ok)throw Error('无法读取服务器记录，请刷新重试。');
    return r.json();
  };
  const write=async record=>libraryPost('record',await packRecord(record));
  return op({getAll:()=>read('records'),get:async id=>unpackRecord(await read('record?id='+encodeURIComponent(id))),add:write,put:write});
}
const libraryTools={
  data(){return {libraryServer,authenticatedPage:location.pathname==='/biology/',backupNotice:'',accountName:'',currentPassword:'',nextPassword:'',confirmPassword:'',accountNotice:'',accountBusy:false,accountLoaded:false}},
  watch:{
    page:{immediate:true,async handler(page){
      this.currentPassword=this.nextPassword=this.confirmPassword='';
      this.accountNotice='';this.accountLoaded=false;
      if(page!=='设置' || !libraryServer || !this.authenticatedPage)return;
      try{
        const response=await fetch('account',{cache:'no-store'});
        if(!response.ok)throw Error(response.status===401?'登录已过期，请刷新页面。':'无法读取账号，请刷新重试。');
        this.accountName=(await response.json()).username;this.accountLoaded=true;
      }catch(error){this.accountNotice=error.message}
    }}
  },
  methods:{
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
