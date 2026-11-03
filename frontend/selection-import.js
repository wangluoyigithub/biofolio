// Read only embedded Word content. Build display HTML from an allow-list of tags.
const escapeDoc = text => text.replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;');
const partsNamed = (el, name) => Array.from(el.getElementsByTagNameNS('*',name));
const docText = el => partsNamed(el,'t').map(n=>n.textContent).join('');
const imageData = blob => new Promise((resolve,reject)=>{
  const reader=new FileReader();reader.onload=()=>resolve(reader.result);reader.onerror=reject;reader.readAsDataURL(blob);
});

async function parsePaper(buffer) {
  const zip=XLSX.CFB.read(new Uint8Array(buffer),{type:'array'});
  const entry=path=>XLSX.CFB.find(zip,'/'+path)?.content;
  const xml=path=>{
    const bytes=entry(path);if(!bytes)throw Error('找不到 Word 正文，请使用 DOCX 文件。');
    const doc=new DOMParser().parseFromString(new TextDecoder().decode(bytes),'application/xml');
    if(doc.querySelector('parsererror'))throw Error('Word 文档结构无法读取。');
    return doc;
  };
  const doc=xml('word/document.xml');
  const rels=new Map(entry('word/_rels/document.xml.rels')?partsNamed(xml('word/_rels/document.xml.rels'),'Relationship')
    .filter(r=>r.getAttribute('TargetMode')!=='External').map(r=>[r.getAttribute('Id'),r.getAttribute('Target')]):[]);
  const images=new Map();
  for(const [id,path] of rels){
    if(!/^media\/[^/]+\.(png|jpe?g|gif)$/i.test(path))continue;
    const bytes=entry('word/'+path);if(bytes)images.set(id,await imageData(new Blob([bytes],{type:'image/'+path.split('.').pop().toLowerCase().replace('jpg','jpeg')})));
  }
  function render(el){
    const tag=el.localName;
    if(tag==='t')return escapeDoc(el.textContent);
    if(tag==='br')return '<br>';
    if(tag==='tab')return '　';
    if(tag==='AlternateContent')return el.firstElementChild?render(el.firstElementChild):'';
    if(tag==='imagedata'||tag==='blip'){
      const rid=el.getAttribute('r:id')||el.getAttribute('r:embed');
      return images.has(rid)?'<img alt="原卷配图" src="'+images.get(rid)+'">':'〔图片需核对原卷〕';
    }
    const content=Array.from(el.children,render).join('');
    if(tag==='r'){
      const pos=partsNamed(el,'vertAlign')[0]?.getAttribute('w:val');
      if(pos==='superscript')return '<sup>'+content+'</sup>';
      if(pos==='subscript')return '<sub>'+content+'</sub>';
    }
    const wrap={p:'p',tbl:'table',tr:'tr',tc:'td'}[tag];
    return wrap&&content?'<'+wrap+'>'+content+'</'+wrap+'>':content;
  }
  const found=new Map();let current=null,field='content',type='待定题型';
  for(const el of partsNamed(doc,'body')[0].children){
    const line=docText(el).trim();
    if(/^[一二三四五六七八九十]+[、．.]/.test(line)){
      type=/非选择/.test(line)?'非选择题':/多[项选]/.test(line)?'多选题':/单[项选]|选择题/.test(line)?'单选题':/实验/.test(line)?'实验题':/判断/.test(line)?'判断题':/填空/.test(line)?'填空题':'非选择题';current=null;continue;
    }
    const number=/^(\d+)[.．、]/.exec(line);
    if(number){
      const n=Number(number[1]);if(found.has(n))throw Error('检测到重复题号 '+n+'，请先将试题统一连续编号。');
      current={n,type,stem:line.slice(number[0].length).trim(),content:'',answer:'',analysis:'',plain:''};found.set(n,current);field='content';
    }
    if(!current)continue;
    let html=render(el);
    const marker=/^(?:【(答案|解析)】|(答案|解析)(?=[\s　：:]|$))[\s　：:]*/.exec(line);
    if(marker){field=(marker[1]||marker[2])==='答案'?'answer':'analysis';html=html.replace(marker[0].trimEnd(),'')}
    current[field]+=html;
    if(field==='content')current.plain+=line+'\n';
  }
  return found;
}

function paperDB(){
  return new Promise((resolve,reject)=>{
    const r=indexedDB.open(key+'-imports',1);
    r.onupgradeneeded=()=>r.result.createObjectStore('records',{keyPath:'id'});
    r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error);
  });
}
async function accessRecords(mode,operation){
  if(libraryServer)return serverRecords(operation);
  if(libraryOffline && mode==='readonly'){
    const base=libraryOffline.records||[];
    const local=await browserRecords(mode,operation);
    if(Array.isArray(local)){
      const ids=new Set(local.map(r=>r.id));return [...base.filter(r=>!ids.has(r.id)).map(unpackRecord),...local]
    }
    return local || operation({get:id=>unpackRecord(base.find(r=>r.id===id))});
  }
  return browserRecords(mode,operation);
}
async function browserRecords(mode,operation){
  const db=await paperDB();
  try{return await new Promise((resolve,reject)=>{
    const tx=db.transaction('records',mode),req=operation(tx.objectStore('records'));
    tx.oncomplete=()=>resolve(req.result);tx.onabort=()=>reject(tx.error||Error('浏览器未保存数据'));
  })}finally{db.close()}
}

const isPaperPhoto = file => /^(image\/(jpeg|png|webp))$/i.test(file.type) || /\.(jpe?g|png|webp)$/i.test(file.name);
async function fileDigest(bytes) {
  if(!crypto.subtle && libraryServer){
    const encoded=(await imageData(new Blob([bytes]))).split(',')[1];
    const response=await fetch('library/digest',{method:'POST',headers:{'Content-Type':'application/json','X-Biology-Library':'1'},body:JSON.stringify(encoded)});
    if(!response.ok)throw Error('题库服务未能校验文件。');
    return response.json();
  }
  const digest=await crypto.subtle.digest('SHA-256',bytes);
  return Array.from(new Uint8Array(digest),v=>v.toString(16).padStart(2,'0')).join('');
}
const paperImports={
  data(){return {photoPages:[],viewPhotos:[],showSolution:false,paperNotes:saved.paperNotes||{},noteId:"",noteDraft:"",viewPaperId:"",viewFilename:"",originalUrl:"",viewMessage:"",wordPage:"",pdfPage:false,viewToken:0,aliases:saved.aliases||{},renameId:'',renameText:'',importName:'',importOrigin:'',original:null,solution:null,importRows:[],pick:[],importId:'',busyImport:false,importMsg:'',readyImport:false,duplicateId:'',photo:null,singlePaper:'',singleNo:'',singleError:''}},
  async mounted(){
    try{const records=await accessRecords('readonly',s=>s.getAll());for(const r of records.filter(r=>!r.before))this.attachRecord(r);for(const r of records.filter(r=>r.before).sort((a,b)=>a.question.revision-b.question.revision))this.applyRevision(r)}
    catch{this.notify('浏览器未能读取本地导入记录。')}
  },
  methods:{
    paperLabel(id){return this.aliases[id]||id},
    attachRecord(r){
      if(r.wordTemplate){
        if(!this.formats.some(f=>f.id===r.id))this.formats.push({id:r.id,name:r.name});
        return;
      }
      if(r.paper&&!this.papers.includes(r.id))this.papers.unshift(r.id);
      if(r.paper)this.aliases[r.id]??=r.name;
      for(const row of r.links||[])if(!this.linkedRows.some(old=>old.q.id===row.q.id))this.linkedRows.push(row);
      const ids=new Set(this.qs.map(q=>q.id));
      this.qs.unshift(...r.questions.filter(q=>!ids.has(q.id)));
    },
    startRename(id){this.renameId=id;this.renameText=this.paperLabel(id);this.$refs.rename.showModal()},
    confirmRename(){if(!this.renameText.trim())return;this.aliases[this.renameId]=this.renameText.trim();this.save();this.$refs.rename.close()},
    startImport(){this.previewNumber=null;this.releasePages();Object.assign(this,{original:null,solution:null,showSolution:false,importName:'',importOrigin:'',importRows:[],pick:[],importMsg:'',readyImport:false,duplicateId:''});this.$refs.paperForm.reset();this.$refs.importer.showModal()},
    releasePages(){
      for (const page of this.photoPages) URL.revokeObjectURL(page.url);
      this.photoPages=[];
    },
    resetPreview(){
      this.previewNumber=null;this.readyImport=false;this.duplicateId='';this.importRows=[];this.importMsg='';
    },
    chooseOriginal(event){
      const files=Array.from(event.target.files);
      event.target.value='';
      this.addOriginals(files);
    },
    pastePaper(event){
      const files=Array.from(event.clipboardData?.files||[]);
      if(files.length && !this.busyImport){event.preventDefault();this.addOriginals(files)}
    },
    addOriginals(files){
      if(!files.length)return;
      this.resetPreview();
      const photos=files.every(isPaperPhoto);
      if(!photos && !(files.length===1 && /\.(docx|pdf)$/i.test(files[0].name))){
        this.importMsg='请选择一份 Word 或 PDF，或同一份试卷的 JPG、PNG、WebP 照片。';return;
      }
      if(!photos)this.releasePages();
      this.solution=null;this.showSolution=false;
      if(photos){
        for(const file of files)this.photoPages.push({file,url:URL.createObjectURL(file)});
      }
      this.original=this.photoPages[0]?.file||files[0];
      if(!this.importName.trim())this.importName=this.original.name.replace(/\.[^.]+$/,'');
    },
    changePage(i,offset){
      this.resetPreview();
      if(!offset){URL.revokeObjectURL(this.photoPages[i].url);this.photoPages.splice(i,1)}
      else this.photoPages.splice(i+offset,0,this.photoPages.splice(i,1)[0]);
      this.original=this.photoPages[0]?.file||null;
    },
    chooseSolution(event){
      this.resetPreview();this.solution=event.target.files[0]||null;
    },
    async prepareImport(){
      this.busyImport=true;this.importMsg='';this.readyImport=false;
      try{
        const bytes=await this.original.arrayBuffer();
        let hash=await fileDigest(bytes);
        if(this.photoPages.length>1){
          const hashes=[hash];
          for(const page of this.photoPages.slice(1))hashes.push(await fileDigest(await page.file.arrayBuffer()));
          hash=await fileDigest(new TextEncoder().encode(hashes.join(':')));
        }
        this.importId='paper-'+hash;
        if(hash===dataset?.studentHash||this.papers.includes(this.importId)){
          this.duplicateId=hash===dataset?.studentHash?dataset.originalName.replace(/\.docx$/i,''):this.importId;
          this.importMsg='这份原卷已经导入：'+this.paperLabel(this.duplicateId);return
        }
        if(this.photoPages.length){
          this.readyImport=true;this.importMsg='将按当前页序保存 '+this.photoPages.length+' 张原卷照片。';return;
        }
        if(this.original.name.toLowerCase().endsWith('.pdf')){this.readyImport=true;this.importMsg='此 PDF 将作为原卷保存，可直接打开查看。';return}
        const rows=await parsePaper(bytes);
        const solutions=this.solution?await parsePaper(await this.solution.arrayBuffer()):new Map();
        for(const q of rows.values()){
          const sol=solutions.get(q.n);
          if(sol&&sol.stem.replace(/\s/g,'')!==q.stem.replace(/\s/g,''))throw Error('解析版第 '+q.n+' 题题干不一致，请核对文件。');
          if(sol){q.answer=sol.answer||q.answer;q.analysis=sol.analysis||q.analysis}
        }
        this.matchPaperRows(rows.values());this.importRows=[...rows.values()];this.pick=[...rows.keys()];this.readyImport=true;
        this.importMsg=rows.size?'识别到 '+rows.size+' 道题，可勾选收藏。':'未识别出带题号的段落，仍可保存并查看整份原卷。'
      }catch(e){this.importMsg=e.message}finally{this.busyImport=false}
    },
    async finishImport(){
      if(!this.readyImport||!this.importName.trim())return;
      this.busyImport=true;
      try{
        const time=new Date(),date=time.toLocaleDateString('sv-SE');
        const questions=this.importRows.filter(r=>this.pick.includes(r.n)).map(r=>({
          id:this.importId+'-'+r.n,stem:r.stem,body:r.plain,contentHtml:r.content,answerHtml:r.answer,analysisHtml:r.analysis,a:r.analysis,answer:'',src:[[this.importId,r.n]],origin:this.importOrigin.trim()||r.stem.match(/^(?:[（(]\d+分[)）]\s*)?[（(]((?:19|20)\d{2}\s*[·•・][^（）()]+)[）)]/)?.[1]||'',date,collectedAt:date+'T'+time.toTimeString().slice(0,8),type:r.type,k:[],level:''}));
        const targets=new Map(this.importRows.filter(row=>row.link && row.matchId).map(row=>[row.n,row.matchId]));
        const links=questions.filter(q=>targets.has(q.src[0][1])).map(q=>({q,target:targets.get(q.src[0][1])}));
        const unique=questions.filter(q=>!targets.has(q.src[0][1]));
        const record={id:this.importId,paper:true,name:this.importName.trim(),original:this.photoPages.length?null:this.original,photos:this.photoPages.map(page=>page.file),solution:this.solution,questions:unique,links};
        await accessRecords('readwrite',s=>s.add(record));this.attachRecord(record);this.$refs.importer.close();this.go('已导入试卷');this.notify('已保存原卷，收录 '+questions.length+' 道题');void this.classifyQuestions(unique)
      }catch(e){this.importMsg='导入未完成：'+e.message}finally{this.busyImport=false}
    },
    startNote(id){
      this.noteId=id;
      this.noteDraft=this.paperNotes[id]||'';
      this.$refs.noteDialog.showModal();
    },
    savePaperNote(){
      this.paperNotes[this.noteId]=this.noteDraft.trim();
      this.save();
      this.$refs.noteDialog.close();
    },
    clearOriginal(){
      this.viewToken++;
      for(const page of this.viewPhotos)URL.revokeObjectURL(page.url);
      this.viewPhotos=[];
      if(this.originalUrl)URL.revokeObjectURL(this.originalUrl);
      this.originalUrl='';this.wordPage='';this.pdfPage=false;
    },
    async openOriginal(id){
      this.clearOriginal();
      const token=this.viewToken;
      this.viewPaperId=id;
      this.viewMessage='正在打开原卷…';
      this.$refs.originalDialog.showModal();
      try {
        let file;
        if(dataset?.originalDocx && id===dataset.originalName.replace(/\.docx$/i,'')){
          const bytes=Uint8Array.from(atob(dataset.originalDocx),c=>c.charCodeAt(0));
          file=new File([bytes],dataset.originalName);
        } else {
          const record=await accessRecords('readonly',store=>store.get(id));
          if(token!==this.viewToken)return;
          if(record?.photos?.length){
            this.viewPhotos=record.photos.map(file=>({name:file.name,url:URL.createObjectURL(file)}));
            this.viewMessage='';return;
          }
          file=record?.original;
        }
        if(token!==this.viewToken)return;
        if(!file)throw Error('没有找到这份试卷的原始文件。');
        this.viewFilename=file.name;
        this.pdfPage=/\.pdf$/i.test(file.name);
        this.originalUrl=URL.createObjectURL(this.pdfPage?new Blob([file],{type:'application/pdf'}):file);
        if(!this.pdfPage){
          const encoded=(await imageData(file)).split(',')[1];
          if(token!==this.viewToken)return;
          const lib=document.getElementById('word-libraries');
          if(!lib)throw Error('此页面缺少 Word 阅读器，请下载原卷查看。');
          const scripts=new TextDecoder().decode(Uint8Array.from(atob(lib.textContent),c=>c.charCodeAt(0)));
          this.wordPage=`<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-reader'; style-src 'unsafe-inline'; img-src blob: data:; font-src blob: data:"><style>body{margin:0;background:#eee}p#status{padding:12px;font:16px system-ui}</style><p id="status">正在排版…</p><div id="pages"></div>${scripts}<script nonce="reader">
            const bytes=Uint8Array.from(atob("${encoded}"),c=>c.charCodeAt(0));
            docx.renderAsync(bytes,document.querySelector('#pages'),null,{renderAltChunks:false,useBase64URL:true,ignoreLastRenderedPageBreak:false})
              .then(()=>document.querySelector('#status').remove())
              .catch(()=>document.querySelector('#status').textContent='无法预览此文档，请下载原卷查看。');
          <\/script>`;
        }
        this.viewMessage='';
      } catch(error){if(token===this.viewToken)this.viewMessage=error.message}
    }
  }
};

function cleanEditorHTML(root){
  function serialize(node){
    if(node.nodeType===Node.TEXT_NODE)return escapeDoc(node.textContent);
    const tag=node.nodeName.toLowerCase();
    if(tag==='img'){
      const src=node.getAttribute('src')||'';
      return /^data:image\/(?:png|jpeg|gif|webp);base64,[a-z0-9+/=]+$/i.test(src)?'<img alt="题目配图" src="'+src+'">':'';
    }
    if(tag==='br')return '<br>';
    const inner=Array.from(node.childNodes,serialize).join('');
    if(['div','p','b','i','sup','sub','table','tbody','tr','td','th','u','strong','em'].includes(tag))return '<'+tag+'>'+inner+'</'+tag+'>';
    return inner;
  }
  return Array.from(root.childNodes,serialize).join('');
}

const questionEditing={
  data(){return {historyByKey:{},editType:'',editChapter:'',editTags:'',checkedAnswers:false,editTypes:[],editNotice:'',savingEdit:false}},
  methods:{
    applyRevision(record){
      const idx=this.qs.findIndex(q=>q.id===record.question.id);if(idx<0)return;
      this.historyByKey[record.before.id+'@'+(record.before.revision||0)]=record.before;
      this.qs.splice(idx,1,record.question)
    },
    editionQuestion(id,edition=this.review){
      const version=edition?.versions?.[id]||0;
      return this.historyByKey[id+'@'+version]||this.qs.find(q=>q.id===id);
    },
    startEdit(){
      const q=this.qs.find(row=>row.id===this.detail.id), fix=this.fixes[q.id]||{};
      this.editType=primaryType(q);this.editTypes=[...new Set(['单选题','多选题','实验题','填空题','判断题',this.editType])];
      this.editChapter=q.ch||'';this.editTags=this.knowledgeOf(q).join('、');this.checkedAnswers=false;
      const plain=t=>escapeDoc(t||'').replaceAll('\n','<br>');
      this.$refs.editStem.innerHTML=q.contentHtml||plain(q.stem+(q.body!==q.stem?'\n'+q.body:''));
      this.$refs.editAnswer.innerHTML=q.answerHtml||plain(fix.answer||q.answer);
      this.$refs.editAnalysis.innerHTML=q.analysisHtml||plain(fix.analysis||q.a);
      this.editNotice='';this.$refs.editor.showModal();this.$refs.editStem.focus()
    },
    async commitEdit(){
      if(this.savingEdit)return;
      const el=this.$refs.editStem;
      if(!el.innerText.trim()&&!el.querySelector('img')){this.editNotice='题面不能为空。';return}
      this.savingEdit=true;
      try{
        const before=JSON.parse(JSON.stringify(this.qs.find(row=>row.id===this.detail.id)));
        const fix=this.fixes[before.id]||{};
        if(!before.answerHtml&&fix.answer)before.answer=fix.answer;
        if(!before.analysisHtml&&fix.analysis)before.a=fix.analysis;
        const body=el.innerText.trim(),revision=(before.revision||0)+1;
        const rich=node=>node.innerText.trim()||node.querySelector('img')?cleanEditorHTML(node):'';
        const tags=[...new Set(this.editTags.split(/[、,，;；\n]/).map(t=>t.trim()).filter(Boolean))];
        const question={...before,revision,body,classificationReviewed:true,ch:this.editChapter,k:tags,type:this.editType,category:this.editType,stem:body.split('\n').find(line=>line.trim())||'图片题',contentHtml:rich(el),
          answer:this.$refs.editAnswer.innerText.trim(),answerHtml:rich(this.$refs.editAnswer),
          a:this.$refs.editAnalysis.innerText.trim(),analysisHtml:rich(this.$refs.editAnalysis)};
        const record={id:'revision:'+before.id+':'+revision,question,before};
        await accessRecords('readwrite',s=>s.add(record));
        delete fix.answer;delete fix.analysis;delete fix.knowledge;
        if(this.checkedAnswers && question.answer)this.fixes[before.id]={...fix,checkedLinks:this.linkedTo(before).map(row=>row.q.id)};
        this.applyRevision(record);this.detail=this.page==='阅读试卷'?this.editionQuestion(question.id):question;this.save();this.$refs.editor.close();this.notify('题目修改已保存');
      }catch(e){this.editNotice='保存失败，请刷新后核对版本。'+e.message}finally{this.savingEdit=false}
    },
    pastePlain(event){
      event.preventDefault();document.execCommand('insertText',false,event.clipboardData.getData('text/plain'))
    }
  }
};
