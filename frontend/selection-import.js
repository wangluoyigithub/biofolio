// Read only embedded Word content. Build display HTML from an allow-list of tags.
const escapeDoc = text => text.replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;');
const partsNamed = (el, name) => Array.from(el.getElementsByTagNameNS('*',name));
const docText = el => partsNamed(el,'t').map(n=>n.textContent).join('');
// A practice-set title such as 课时精练8. Workbooks often typeset it as an EQ
// field, so its text sits in instrText rather than in ordinary runs.
function sectionMark(el,line){
  const raw=(line||partsNamed(el,'instrText').map(n=>n.textContent).join(''))
    .replace(/^\s*eq\s+(?:\\[a-z]+\d*\s*)*\(/i,'').replace(/\)\s*$/,'').replace(/[\s　]+/g,'');
  return /^(?:课时(?:精练|作业|练习|训练|检测)|课后(?:练习|作业|精练|训练)|分层(?:作业|训练|练习)|随堂(?:演练|练习|检测))[0-9一二三四五六七八九十]*$/.test(raw)?raw:'';
}
const imageData = blob => new Promise((resolve,reject)=>{
  const reader=new FileReader();reader.onload=()=>resolve(reader.result);reader.onerror=reject;reader.readAsDataURL(blob);
});

function hasSolution(value){
  if(!value)return false;
  if(!/[<&]/.test(value))return !!value.trim() && !['待补答案','待补解析'].includes(value.trim());
  const el=document.createElement('template');el.innerHTML=value;
  const text=el.content.textContent.trim();
  return (!!text && !['待补答案','待补解析'].includes(text)) || Array.from(el.content.querySelectorAll('img'),i=>i.getAttribute('src')).some(src=>src?.startsWith('data:image/')||libraryPicture(src));
}
function solutionReady(row){
  return (hasSolution(row.answer)||hasSolution(row.addAnswer)) && (hasSolution(row.analysis)||hasSolution(row.addAnalysis));
}

async function parsePaper(buffer, allowUnnumbered=false) {
  await loadTool('xlsx');
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
  const found=new Map(),candidates=[];let current=null,field='content',type='待定题型',exercise=false,pendingImages='',section='',skipping=false;
  const paragraphs=Array.from(partsNamed(doc,'body')[0].children);
  const texts=paragraphs.map(el=>docText(el).trim());
  const mixed=texts.some(t=>/^课时学习目标|^学习目标|^知识梳理|^例\s*\d+/.test(t));
  const startNumber=line=>/^(例\s*)(\d+)\s*[.．、]?\s*/.exec(line)||/^(\d+)[.．、]\s*/.exec(line);
  const unnumbered=allowUnnumbered && !texts.some(startNumber);
  for(const [index,el] of paragraphs.entries()){
    const line=texts[index];
    const mark=sectionMark(el,line);
    if(mark){section=mark;exercise=true;skipping=false;current=null;pendingImages='';continue}
    // Recitation lists and long-answer drills restart their own numbering and
    // are not test items, so skip them until the next heading.
    if(line.length<=24 && /晨读必背|知识清单|练长句|长句表达|^必背/.test(line)){skipping=true;current=null;pendingImages='';continue}
    const heading=/^[一二三四五六七八九十]+(?:[、．.]|[\s　])+[^\n]+$/.test(line);
    const divider=/^【(?:基础对点|综合提升|能力提升|课时精练|知识梳理|思维导图|学习目标)】|^[（(]时间[：:]|^课时学习目标|^第[一二三四五六七八九十\d]+[章节]/.test(line);
    if(heading||divider){
      if(/基础对点|综合提升|能力提升|课时精练|^[（(]时间/.test(line))exercise=true;
      if(/知识梳理|思维导图|学习目标/.test(line))exercise=false;
      if(/非选择|多[项选]|单[项选]|选择题|实验题|判断题|填空题/.test(line))type=/非选择/.test(line)?'非选择题':/多[项选]/.test(line)?'多选题':/选择题|单[项选]/.test(line)?'单选题':/实验/.test(line)?'实验题':/判断/.test(line)?'判断题':'填空题';
      skipping=false;current=null;pendingImages='';continue;
    }
    if(skipping && line.length<=40 && /测评|对点|提升|精练|训练|检测|选择题|练习/.test(line)){skipping=false;exercise=true;current=null;pendingImages='';continue}
    if(skipping)continue;
    const number=startNumber(line);
    if(number){
      if(current)current[field]+=pendingImages;pendingImages='';
      const example=number.length===3,n=example?'例'+Number(number[2]):Number(number[1]);
      current={n,type,stem:line.slice(number[0].length).trim(),content:'',answer:'',analysis:'',plain:'',example,exercise,section:example?'课堂例题':section||(exercise?'课后练习':'')};candidates.push(current);field='content';
    }
    if(!current && unnumbered && (line || partsNamed(el,'blip').length || partsNamed(el,'imagedata').length)){
      current={n:1,type,stem:line,content:'',answer:'',analysis:'',plain:'',exercise:true};candidates.push(current);field='content';
    }
    if(!current)continue;
    let html=render(el);
    if(!line && /<img\b/.test(html) && field!=='content'){pendingImages+=html;continue}
    if(!line && !/<img\b/.test(html))continue;
    current[field]+=pendingImages;pendingImages='';
    const marker=/^(?:【(答案|解析)】|(答案|解析)(?=[\s　：:]|$))[\s　：:]*/.exec(line);
    if(marker){field=(marker[1]||marker[2])==='答案'?'answer':'analysis';html=html.replace(marker[0].trimEnd(),'')}
    current[field]+=html;
    if(field==='content')current.plain+=line+'\n';
  }
  if(current)current[field]+=pendingImages;
  for(const row of candidates){
    const options=/(?:^|\n|\s)[A-D][.．、]/.test(row.plain)||/<td>\s*[A-D]\s*<\/td>/.test(row.content);
    const isQuestion=options||hasSolution(row.answer)||['判断题','填空题','实验题','单选题','多选题','非选择题'].includes(row.type)||/[？?]|[（(]\s*[）)]|_{2,}|下列|回答|请|计算|说明|分析|概述|简述|比较|写出|说出|什么|为何|是否|如何/.test(row.plain);
    if(!unnumbered && (!isQuestion || (mixed&&!row.example&&!row.exercise&&!hasSolution(row.answer)&&!hasSolution(row.analysis))))continue;
    if(found.has(row.n))throw Error('检测到重复题号 '+row.n+'，请将同类题目统一连续编号。');
    if(row.type==='待定题型'){
      const answer=document.createElement('template');answer.innerHTML=row.answer;
      row.type=/多选/.test(row.stem)||/^[A-H]{2,}$/.test(answer.content.textContent.trim())?'多选题':options||/^[A-H]$/.test(answer.content.textContent.trim())?'单选题':'非选择题';
    }
    delete row.example;delete row.exercise;found.set(row.n,row);
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
  data(){return {reviewablePapers:{},draftVersion:0,startBrief:currentPage()==='首页',briefRecords:false,photoPages:[],viewPhotos:[],showSolution:false,paperNotes:saved.paperNotes||{},noteId:"",noteDraft:"",viewPaperId:"",viewFilename:"",originalUrl:"",viewMessage:"",wordPage:"",pdfPage:false,viewToken:0,aliases:saved.aliases||{},renameId:'',renameText:'',importName:'',importOrigin:'',original:null,solution:null,importRows:[],pick:[],batchFiles:[],batchBusy:false,dragDepth:0,importId:'',busyImport:false,importMsg:'',readyImport:false,duplicateId:'',photo:null,singlePaper:'',singleNo:'',singleError:''}},
  mounted(){
    this.initialRecords=this.loadInitialRecords().catch(error=>{
      this.notify(error.message);
      if(libraryServer){this.initialRecords=null;this.briefRecords=true}
    });
  },
  methods:{
    async loadInitialRecords(){
      const records=libraryServer && this.startBrief
        ? await (await readAsset('library/records?brief=1')).json()
        : await accessRecords('readonly',store=>store.getAll());
      this.briefRecords=!!libraryServer && this.startBrief;
      this.applyRecords(records);
    },
    applyRecords(records){
      for(const r of records.filter(r=>!r.before))this.attachRecord(r);
      for(const r of records.filter(r=>r.before).sort((a,b)=>a.question.revision-b.question.revision))this.applyRevision(r);
    },
    hasSolution,solutionReady,
    paperNumber(n){return /^例/.test(String(n))?String(n):'第 '+n+' 题'},
    paperLabel(id){return this.aliases[id]||id},
    attachRecord(r){
      if(r.wordTemplate){
        if(!this.formats.some(f=>f.id===r.id))this.formats.push({id:r.id,name:r.name});
        return;
      }
      if(r.paper&&!this.papers.includes(r.id))this.papers.unshift(r.id);
      if(r.paper)this.aliases[r.id]??=r.name;
      if(r.paper && r.questionCount)this.reviewablePapers[r.id]=r.questionCount;
      for(const row of r.links||[])if(!this.linkedRows.some(old=>old.q.id===row.q.id))this.linkedRows.push(row);
      const ids=new Set(this.qs.map(q=>q.id));
      for(const q of r.questions)q.uploadedBy=r.uploadedBy||'';
      this.qs.unshift(...r.questions.filter(q=>!ids.has(q.id)));
    },
    startRename(id){this.renameId=id;this.renameText=this.paperLabel(id);this.$refs.rename.showModal()},
    confirmRename(){if(!this.renameText.trim())return;const id=this.renameId,old=this.aliases[id];this.aliases[id]=this.renameText.trim();if(!this.persist(()=>{this.aliases[id]=old}))return;this.$refs.rename.close()},
    importSections(){
      const groups=new Map();
      for(const r of this.importRows){
        if(r.stored)continue;
        const name=r.section||'其他题目',g=groups.get(name)||{name,total:0,ready:0};
        g.total++;if(solutionReady(r))g.ready++;groups.set(name,g);
      }
      return groups.size>1?[...groups.values()]:[];
    },
    sectionLabel(g){return '只选'+g.name+'（'+(g.ready===g.total?g.total:g.ready+' / '+g.total)+' 题）'},
    jumpRow(n){const el=document.getElementById('intake-row-'+n);if(!el)return;const nav=document.querySelector('.intake-nav');window.scrollTo(0,el.getBoundingClientRect().top+window.scrollY-(nav?nav.offsetHeight+12:12));el.focus({preventScroll:true})},
    pickSection(name){
      this.pick=this.importRows.filter(r=>!r.stored&&solutionReady(r)&&(name===null||(r.section||'其他题目')===name)).map(r=>r.n);
    },
    resetImport(){this.previewNumber=null;this.releasePages();Object.assign(this,{original:null,solution:null,showSolution:false,importName:'',importOrigin:'',importRows:[],pick:[],importMsg:'',readyImport:false,duplicateId:''})},
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
      if(this.batchFiles.length || (files.length>1 && files.every(f=>/\.(docx|pdf)$/i.test(f.name)))){this.queueBatch(files);return}
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
    dropPapers(event){
      this.dragDepth=0;
      const files=Array.from(event.dataTransfer?.files||[]);
      if(!files.length || this.busyImport || this.batchBusy)return;
      if(this.readyImport && this.importRows.some(r=>!r.stored) && !confirm('当前试卷还没核完，换成拖入的文件吗？未保存的核题内容会丢失。'))return;
      this.addOriginals(files);
    },
    queueBatch(files){
      if(this.batchBusy)return;
      const bad=files.filter(f=>!/\.(docx|pdf)$/i.test(f.name));
      if(bad.length){this.importMsg='批量添加只支持 Word 和 PDF：'+bad.map(f=>f.name).join('、');return}
      this.resetImport();
      for(const file of files){
        const key=file.name+':'+file.size+':'+file.lastModified;
        if(!this.batchFiles.some(b=>b.key===key))this.batchFiles.push({key,file,name:file.name.replace(/\.[^.]+$/,''),status:'待添加',note:''});
      }
    },
    removeBatch(item){if(!this.batchBusy)this.batchFiles=this.batchFiles.filter(b=>b!==item)},
    clearBatch(){if(!this.batchBusy){this.batchFiles=[];this.resetImport()}},
    async runBatch(){
      if(this.batchBusy)return;
      this.batchBusy=true;
      try{
        for(const item of this.batchFiles){
          if(['已添加','已导入过'].includes(item.status))continue;
          item.status='处理中';item.note='';
          try{
            this.resetImport();
            this.original=item.file;this.importName=item.name.trim()||item.file.name.replace(/\.[^.]+$/,'');
            await this.prepareImport();
            if(this.duplicateId || this.papers.includes(this.importId)){
              item.status='已导入过';item.note=this.paperLabel(this.duplicateId||this.importId);continue;
            }
            if(!this.readyImport)throw Error(this.importMsg||'无法读取这份文件');
            await this.ensureOriginal();
            item.status='已添加';
            item.note=this.importRows.length?'识别出 '+this.importRows.length+' 题，待核题':'已保存原卷';
          }catch(error){item.status='失败';item.note=error.message||'添加失败'}
        }
      }finally{
        this.resetImport();this.batchBusy=false;
        const done=this.batchFiles.filter(b=>b.status==='已添加').length;
        this.notify('已添加 '+done+' 份试卷');
      }
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
        const existing=this.papers.includes(this.importId);
        if(hash===dataset?.studentHash || (existing && !/\.docx$/i.test(this.original.name))){
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
        const savedNumbers=new Set(this.qs.flatMap(q=>this.sourcesOf(q)).filter(([id])=>id===this.importId).map(([,n])=>String(n)));
        this.importRows=[...rows.values()];
        for(const r of this.importRows)r.stored=savedNumbers.has(String(r.n));
        const remaining=this.importRows.filter(r=>!r.stored);
        if(existing && !remaining.length){this.importRows=[];this.duplicateId=this.importId;this.importMsg='这份原卷已全部收录。';return}
        await this.matchPaperRows(remaining);
        this.pick=remaining.filter(solutionReady).map(r=>r.n);
        this.importMsg=rows.size?'仅提取题目，共 '+rows.size+' 道；答案和解析齐全的题才可勾选。':'未识别出带题号的段落，仍可保存并查看整份原卷。';
        await this.readReviewDraft();this.readyImport=true
      }catch(e){this.importMsg=e.message}finally{this.busyImport=false}
    },
    async addImportRecord(record){
      try{await accessRecords('readwrite',s=>s.add(record))}
      catch(error){
        // A previous request or another teacher may have committed this same
        // file/number. Reflect the stored result without overwriting their work.
        const saved=await accessRecords('readonly',s=>s.get(record.id));
        if(!saved || saved.id!==record.id)throw error;
        this.attachRecord(saved);return false;
      }
      this.attachRecord(record);return true;
    },
    async resumeImport(id,route){
      const r=await accessRecords('readonly',s=>s.get(id));
      if(location.hash!==route)return;
      if(!r?.original || !r.paper)throw Error('无法读取已保存的原卷，请重新选择文件。');
      this.resetImport();this.original=r.original;this.solution=r.solution;
      this.importName=this.paperLabel(id);this.importOrigin=r.origin||'';
      this.showSolution=!!r.solution;
      await this.prepareImport();
    },
    async readReviewDraft(){
      const draft=libraryServer?await (await readAsset('library/review-draft?paper='+encodeURIComponent(this.importId))).json()
        :JSON.parse(localStorage.getItem(key+':draft:'+this.importId)||'null');
      this.draftVersion=draft?.version||0;
      if(!draft)return;
      this.importOrigin=draft.origin||this.importOrigin;
      for(const row of this.importRows){
        const note=draft.rows?.[row.n];
        if(row.stored||!note)continue;
        if(!hasSolution(row.answer))row.addAnswer=note.answer;
        if(!hasSolution(row.analysis))row.addAnalysis=note.analysis;
        row.link=!!row.matchId && row.matchId===note.link;
      }
      this.pick=this.importRows.filter(r=>!r.stored && draft.pick?.includes(r.n) && solutionReady(r)).map(r=>r.n);
      this.importMsg='已恢复上次保存的个人核题草稿。';
    },
    async writeReviewDraft(){
      const draft={paper:this.importId,version:this.draftVersion+1,origin:this.importOrigin,pick:this.pick,rows:{}};
      for(const r of this.importRows)if(!r.stored)draft.rows[r.n]={answer:r.addAnswer||'',analysis:r.addAnalysis||'',link:r.link?r.matchId:''};
      if(libraryServer)await libraryPost('review-draft',draft);
      else localStorage.setItem(key+':draft:'+this.importId,JSON.stringify(draft));
      this.draftVersion=draft.version;
    },
    async saveReviewDraft(leave){
      if(this.busyImport || !this.readyImport || !this.importName.trim())return;
      this.busyImport=true;
      try{
        await this.ensureOriginal();
        await this.writeReviewDraft();
        this.importMsg='个人核题草稿已保存。';this.notify(this.importMsg);
        if(leave)this.go('已导入试卷');
      }catch(error){this.importMsg=error.message;this.notify(error.message)}
      finally{this.busyImport=false}
    },
    async ensureOriginal(){
      if(this.papers.includes(this.importId))return;
      const record={id:this.importId,paper:true,name:this.importName.trim(),questionCount:this.importRows.length,origin:this.importOrigin.trim(),original:this.photoPages.length?null:this.original,photos:this.photoPages.map(p=>p.file),solution:this.solution,questions:[],links:[]};
      await this.addImportRecord(record);
    },
    async storeImportRow(r){
      if(r.stored)return;
      if(!solutionReady(r))throw Error('第 '+r.n+' 题缺答案或解析');
      const time=new Date(),date=time.toLocaleDateString('sv-SE');
      const q={id:this.importId+'-'+r.n,stem:r.stem,body:r.plain,contentHtml:r.content,answerHtml:hasSolution(r.answer)?r.answer:'',analysisHtml:hasSolution(r.analysis)?r.analysis:'',a:hasSolution(r.analysis)?r.analysis:r.addAnalysis.trim(),answer:hasSolution(r.answer)?'':r.addAnswer.trim(),src:[[this.importId,r.n]],origin:this.importOrigin.trim()||printedQuestionSource(r.stem),date,collectedAt:date+'T'+time.toTimeString().slice(0,8),type:r.type,k:[],level:''};
      const link=r.link && r.matchId;
      const record={id:q.id,questions:link?[]:[q],links:link?[{q,target:r.matchId}]:[]};
      const inserted=await this.addImportRecord(record);
      r.stored=true;this.pick=this.pick.filter(n=>n!==r.n);
      if(inserted&&!link)void this.classifyQuestions([q]);
    },
    async confirmImportRow(row){
      if(this.busyImport || !this.readyImport || !this.importName.trim())return;
      this.busyImport=true;
      try{
        await this.ensureOriginal();await this.storeImportRow(row);
        this.importMsg='第 '+row.n+' 题已保存。';
        const rest=this.importRows.filter(r=>!r.stored);
        const next=rest.find(r=>r.n>row.n)||rest[0];
        if(next)this.$nextTick(()=>document.getElementById('intake-row-'+next.n)?.focus());
      }catch(e){this.importMsg='收录失败：'+e.message;this.notify(this.importMsg)}
      finally{this.busyImport=false}
    },
    async finishImport(){
      if(this.busyImport||!this.readyImport||!this.importName.trim())return;
      this.busyImport=true;
      try{
        const selected=this.importRows.filter(r=>this.pick.includes(r.n)&&!r.stored);
        if(selected.some(r=>!solutionReady(r)))throw Error('所选题目仍缺答案或解析');
        await this.ensureOriginal();
        for(const row of selected)await this.storeImportRow(row);
        if(this.importRows.some(row=>!row.stored))await this.writeReviewDraft();
        this.go('已导入试卷');this.notify('原卷和已确认的题目已保存');
      }catch(e){this.importMsg='导入未完成：'+e.message}finally{this.busyImport=false}
    },
    startNote(id){
      this.noteId=id;
      this.noteDraft=this.paperNotes[id]||'';
      this.$refs.noteDialog.showModal();
    },
    savePaperNote(){
      const id=this.noteId,old=this.paperNotes[id];
      this.paperNotes[id]=this.noteDraft.trim();
      if(!this.persist(()=>{this.paperNotes[id]=old}))return;
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
        if((dataset?.originalUrl||dataset?.originalDocx) && id===dataset.originalName.replace(/\.docx$/i,'')){
          const bytes=dataset.originalUrl?await (await readAsset(dataset.originalUrl)).arrayBuffer():Uint8Array.from(atob(dataset.originalDocx),c=>c.charCodeAt(0));
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
          const scripts=lib.dataset.url?await (await readAsset(lib.dataset.url)).text():new TextDecoder().decode(Uint8Array.from(atob(lib.textContent),c=>c.charCodeAt(0)));
          if(token!==this.viewToken)return;
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
      return (libraryPicture(src)||/^data:image\/(?:png|jpeg|gif|webp);base64,[a-z0-9+/=]+$/i.test(src))?'<img alt="题目配图" src="'+src+'">':'';
    }
    if(tag==='br')return '<br>';
    const inner=Array.from(node.childNodes,serialize).join('');
    if(['div','p','b','i','sup','sub','table','tbody','tr','td','th','u','strong','em'].includes(tag))return '<'+tag+'>'+inner+'</'+tag+'>';
    return inner;
  }
  return Array.from(root.childNodes,serialize).join('');
}

const questionEditing={
  data(){return {editFigureItems:[],figureSource:'',figureTarget:null,editPhotos:[],editPhotoIndex:0,editReference:'',historyByKey:{},editType:'',editChapter:'',editTags:'',checkedAnswers:false,editTypes:[],editNotice:'',savingEdit:false}},
  methods:{
    applyRevision(record){
      const idx=this.qs.findIndex(q=>q.id===record.question.id);if(idx<0)return;
      this.historyByKey[record.before.id+'@'+(record.before.revision||0)]=record.before;
      record.question.uploadedBy=this.qs[idx].uploadedBy;
      record.before.uploadedBy=this.qs[idx].uploadedBy;
      this.qs.splice(idx,1,record.question)
    },
    editionQuestion(id,edition=this.review){
      const version=edition?.versions?.[id]||0;
      return this.historyByKey[id+'@'+version]||this.qs.find(q=>q.id===id);
    },
    startEdit(){
      const q=this.qs.find(row=>row.id===this.detail.id), fix=this.fixes[q.id]||{};
      const face=document.createElement('template');face.innerHTML=q.contentHtml||'';
      this.editPhotos=[...new Set([q.referencePhoto,...Array.from(face.content.querySelectorAll('img'),i=>i.getAttribute('src'))].filter(s=>libraryPicture(s)||s?.startsWith('data:image/')))];
      this.editPhotoIndex=0;this.editReference=q.referencePhoto||'';
      this.editType=primaryType(q);this.editTypes=[...new Set(['单选题','多选题','实验题','填空题','判断题',this.editType])];
      this.editChapter=q.ch||'';this.editTags=this.knowledgeOf(q).join('、');this.checkedAnswers=false;
      const plain=t=>escapeDoc(t||'').replaceAll('\n','<br>');
      this.$refs.editStem.innerHTML=safeHtml(q.contentHtml||plain(q.stem+(q.body!==q.stem?'\n'+q.body:'')));
      this.$refs.editAnswer.innerHTML=safeHtml(q.answerHtml||plain(fix.answer||q.answer));
      this.$refs.editAnalysis.innerHTML=safeHtml(q.analysisHtml||plain(fix.analysis||q.a));
      this.refreshEditFigures();this.editNotice='';this.$refs.editor.showModal();this.$refs.editStem.focus()
    },
    applyEditedPhotoText(draft){
      const el=this.$refs.editStem;el.innerText=draft.text;this.editReference=draft.picture;
      if(draft.includeFigure){const img=document.createElement('img');img.src=draft.figure||draft.picture;img.alt='题目配图';el.append(img)}
      this.refreshEditFigures()
    },
    refreshEditFigures(){this.editFigureItems=Array.from(this.$refs.editStem.querySelectorAll('img'),i=>i.getAttribute('src'))},
    openFigureTool(i){
      const img=this.$refs.editStem.querySelectorAll('img')[i];if(!img)return;
      this.figureTarget=img;this.figureSource=img.getAttribute('src');this.$refs.figureTool.showModal();
    },
    applyFigure(src){
      if(!this.figureTarget || !this.$refs.editStem.contains(this.figureTarget) || this.figureTarget.getAttribute('src')!==this.figureSource){this.notify('题面配图已变化，请重新打开处理。');return}
      this.editReference ||= this.figureSource;this.figureTarget.src=src;this.refreshEditFigures();this.$refs.figureTool.close();this.editNotice='配图已更新到编辑草稿，核对后点「保存修改」。';
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
        if(!hasSolution(rich(this.$refs.editAnswer))||!hasSolution(rich(this.$refs.editAnalysis))){this.editNotice='请补齐答案和解析后再保存。';return}
        const tags=[...new Set(this.editTags.split(/[、,，;；\n]/).map(t=>t.trim()).filter(Boolean))];
        const question={...before,referencePhoto:this.editReference,revision,body,classificationReviewed:true,ch:this.editChapter,k:tags,type:this.editType,category:this.editType,stem:body.split('\n').find(line=>line.trim())||'图片题',contentHtml:rich(el),
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
