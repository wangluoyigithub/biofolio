// Local-only controls for the selection preview.
function byCollectionTime(a,b) {
  return String(b.collectedAt || b.date).localeCompare(String(a.collectedAt || a.date));
}
function primaryType(question) {
  if (['单选题','单项选择题','选择题'].includes(question.type)) return '单选题';
  if (['多选题','多项选择题'].includes(question.type)) return '多选题';
  return question.category || question.type;
}

const basketSections = ['判断题','单项选择题','多项选择题','非选择题'];
function basketSection(q) {
  if (q.type === '判断题') return 0;
  if (['单选题','单项选择题','选择题'].includes(q.type)) return 1;
  if (['多选题','多项选择题'].includes(q.type)) return 2;
  return 3;
}

// Keep exact evidence of a shared figure or a substantial introductory passage.
async function materialKeys(q) {
  const keys = new Set();
  for (const src of aiQuestionParts(q).images) {
    keys.add('图:' + await pictureKey(src));
  }
  const intro = (q.stem || '').normalize('NFKC').replace(/^\s*(?:\d+[.、]\s*)?(?:\(\d+分\)\s*)?(?:\((?:19|20)\d{2}[^)]*\)\s*)?/, '');
  for (const sentence of intro.split(/[。！？\n]/)) {
    const passage = sentence.replace(/\s/g,'').split(/下列|据此|据图回答|请回答|请分析/)[0];
    if (passage.length >= 24 && !/^[A-D][.、]|正确的是|错误的是|符合题意/.test(passage)) keys.add('文:' + passage);
  }
  return keys;
}

function readScoreRows(rows, length) {
  const clean = rows.filter(row => row.some(value => String(value ?? '').trim() !== ''));
  if (clean.length < 2) throw Error('表格需要表头和至少一行数据。');
  const head = clean[0].map(value => String(value ?? '').trim());
  const body = clean.slice(1);
  const number = value => {
    const n = Number(value);
    if (!Number.isInteger(n) || n < 1 || n > length) throw Error('题号超出本卷范围，请核对最终印发顺序。');
    return n;
  };
  const count = value => {
    if (value == null || String(value).trim() === '' || !Number.isInteger(+value) || +value < 0) throw Error('人数须为非负整数。');
    return +value;
  };
  const qCol = head.indexOf('题号'), errCol = head.indexOf('错误人数'), totalCol = head.indexOf('作答人数');
  if (qCol >= 0 && errCol >= 0) {
    const seen = new Set();
    return body.map(row => {
      const n = number(row[qCol]), missed = count(row[errCol]);
      const total = totalCol < 0 || String(row[totalCol] ?? '').trim()==='' ? null : count(row[totalCol]);
      if (seen.has(n) || (total !== null && missed > total)) throw Error('出现重复题号或错误人数大于作答人数。');
      seen.add(n);return {n,missed,total};
    });
  }
  const listCol=head.indexOf('错题号');
  if (listCol>=0) {
    const counts=new Map();
    for (const row of body) {
      const raw=String(row[listCol]??'').trim();
      if(!raw || raw==='无') continue;
      const ids=new Set(raw.split(/[,，、;；\s]+/).filter(Boolean).map(number));
      ids.forEach(n=>counts.set(n,(counts.get(n)||0)+1));
    }
    if(!counts.size) throw Error('表中没有记录错题号。');
    return Array.from(counts,([n,missed])=>({n,missed,total:null}));
  }
  const cols=head.flatMap((v,i)=>{const match=/^(?:第|Q)?\s*(\d+)\s*(?:题)?$/.exec(v);return match?[{i,n:number(match[1])}]:[]});
  const maxRow=body.find(row=>String(row[0]??'').trim()==='满分');
  if (!cols.length || !maxRow) throw Error('未识别表头。请用下方所列的逐题表、错题号表或得分表格式。');
  if(new Set(cols.map(c=>c.n)).size!==cols.length) throw Error('得分表有重复题号。');
  return cols.map(({i,n})=>{
    const max=Number(maxRow[i]);if(!Number.isFinite(max)||max<=0) throw Error('每道题需要正数满分。');
    let missed=0,total=0;
    for(const row of body){
      if(row===maxRow)continue;
      const v=String(row[i]??'').trim();if(!v||v==='缺考')continue;
      const score=Number(v);if(!Number.isFinite(score)||score<0||score>max)throw Error('第 '+n+' 题含异常得分。');
      total++;if(score<max)missed++;
    }
    return {n,missed,total};
  });
}

const selectionTools = {
  data(){return {queuePage:1,appliedPhoto:null,photoPreview:'',photoSourceId:'',newStem:'',newAnswer:'',newAnalysis:'',wordChoices:[],wordNumber:0,fileSequence:0,readingSingle:false,newSource:'',newType:'单选题',pageNo:1,pageSize:5,paperName:'',editions:saved.editions||[],reviewId:'',reviewNumber:0,
    paperPurpose:'',purposeNames:saved.purposeNames||[],newPurpose:'',newExam:false,examPurposes:saved.examPurposes||['考试试卷','周考试卷'],purposeError:'',purposeFilter:'',formats:[],formatMessage:'',formatBusy:false,wordBusy:false,wordNotice:{},fixes:saved.fixes||{},editField:'',editValue:'',helper:'',scoreInput:'',scoreMessage:'',scorePreview:[],scoreBook:null,sheet:'',sheets:[],scoreTab:false}},
  computed:{
    formalQuestions(){return this.qs.filter(q=>this.bankReady(q))},
    singleComplete(){return solutionReady({answer:this.singleWord?.answer,analysis:this.singleWord?.analysis,addAnswer:this.newAnswer,addAnalysis:this.newAnalysis})},
    singleWord(){return this.wordChoices.find(row=>row.n===this.wordNumber)},
    pendingQuestions(){return this.qs.filter(q=>!this.bankReady(q)).sort(byCollectionTime)},
    queuePages(){return Math.max(1,Math.ceil(this.pendingQuestions.length/this.pageSize))},
    pageCount(){return Math.max(1,Math.ceil(this.matches.length/this.pageSize))},
    visibleQuestions(){const start=(this.pageNo-1)*this.pageSize;return this.matches.slice(start,start+this.pageSize)},
    purposes(){return [...new Set([...this.purposeNames,...this.editions.map(e=>e.purpose).filter(Boolean)])]},
    filterPurposes(){return this.editions.some(e=>!e.purpose)?[...new Set([...this.purposes,'未分类'])]:this.purposes},
    filteredEditions(){return this.editions.filter(e=>!this.purposeFilter||(e.purpose||'未分类')===this.purposeFilter)},
    review(){return this.editions.find(p=>p.id===this.reviewId)},
    filterKey(){return JSON.stringify([this.page,this.mode,this.bookIndex,this.search,this.node,this.type,this.order,this.source,this.flags])}
  },
  watch:{photo(file){this.appliedPhoto=null;if(this.photoPreview)URL.revokeObjectURL(this.photoPreview);this.photoPreview=file?URL.createObjectURL(file):''},queuePages(n){this.queuePage=Math.min(n,this.queuePage)},filterKey(){this.pageNo=1},pageCount(n){this.pageNo=Math.min(n,this.pageNo)}},
  methods:{
    originOf(q){return this.fixes[q.id]?.source ?? q.origin ?? ''},
    startQuestion(paper=''){this.photoSourceId='';this.clearPdf();this.fileSequence++;this.readingSingle=false;this.wordChoices=[];this.wordNumber=0;this.singleMatch=null;this.newStem=this.newAnswer=this.newAnalysis='';this.newSource='';this.newType='单选题';this.photo=null;this.singleError='';this.singlePaper=paper;this.singleNo='';this.$refs.collector.querySelector('form').reset();this.$refs.collector.showModal()},
    async collectFromPhoto(q){
      const src=aiQuestionParts(q).images[0];
      if(!src){this.notify('未找到原照片');return}
      this.startQuestion();this.photoSourceId=q.id;this.readingSingle=true;
      const seq=this.fileSequence;
      try{
        const blob=await (await readAsset(src)).blob();
        if(seq===this.fileSequence)this.photo=new File([blob],q.stem,{type:blob.type});
      }catch(e){if(seq===this.fileSequence)this.singleError='读取原照片失败：'+e.message}
      finally{if(seq===this.fileSequence)this.readingSingle=false}
    },
    escapeFace(text){return '<p>'+escapeDoc(text)+'</p>'},
    solutionText(html,addition){
      if(!hasSolution(html))return (addition||'').trim();
      return aiQuestionParts({contentHtml:html}).text||'已有配图';
    },
    applyImportSolution(row,draft){if(draft.answer)row.addAnswer=draft.answer;if(draft.analysis)row.addAnalysis=draft.analysis},
    applySingleSolution(draft){if(draft.answer)this.newAnswer=draft.answer;if(draft.analysis)this.newAnalysis=draft.analysis},
    applyNewPhotoText(draft){this.newStem=draft.text;this.appliedPhoto=draft;this.singleMatch=null},
    async takeHomePhoto(event){
      const file=event.target.files[0];event.target.value='';if(!file)return;
      this.startQuestion();
      const seq=++this.fileSequence;
      if(['image/jpeg','image/png','image/webp'].includes(file.type)){
        this.photo=file;return;
      }
      this.readingSingle=true;
      const url=URL.createObjectURL(file);
      try{
        const image=new Image();image.src=url;await image.decode();
        const canvas=document.createElement('canvas');canvas.width=image.naturalWidth;canvas.height=image.naturalHeight;
        canvas.getContext('2d').drawImage(image,0,0);
        const jpeg=await new Promise(resolve=>canvas.toBlob(resolve,'image/jpeg',0.94));
        if(!jpeg)throw Error('照片转换失败');
        if(seq===this.fileSequence)this.photo=new File([jpeg],file.name.replace(/\.[^.]+$/,'')+'.jpg',{type:'image/jpeg'});
      }catch{if(seq===this.fileSequence)this.singleError='浏览器无法读取这张照片，请用 JPEG、PNG 或 WebP 格式。'}
      finally{URL.revokeObjectURL(url);if(seq===this.fileSequence)this.readingSingle=false}
    },
    pasteSinglePhoto(event){
      if(this.photoSourceId)return;
      const file=Array.from(event.clipboardData?.files||[]).find(isPaperPhoto);
      if(file){event.preventDefault();this.clearPdf();this.fileSequence++;this.readingSingle=false;this.wordChoices=[];this.singleMatch=null;this.photo=file;this.singleError=''}
    },
    async chooseQuestionFile(event){
      const file=event.target.files[0];if(!file)return;
      this.clearPdf();const seq=++this.fileSequence;
      this.newAnswer=this.newAnalysis='';this.photo=null;this.singleMatch=null;this.wordChoices=[];this.wordNumber=0;this.singleError='';this.readingSingle=true;
      try{
        if(/\.pdf$/i.test(file.name)){await this.openSinglePdf(file,seq)}
        else if(/\.docx$/i.test(file.name)){
          const rows=await parsePaper(await file.arrayBuffer(),true);
          if(seq!==this.fileSequence)return;
          this.wordChoices=[...rows.values()].filter(r=>r.plain.trim()||r.content.includes('<img'));
          if(!this.wordChoices.length)throw Error('没有可收录的题面');
          this.selectWord(this.wordChoices[0].n);
        }else if(/\.txt$/i.test(file.name)){
          const text=await file.text();if(seq===this.fileSequence)this.newStem=text;
        }else if(isPaperPhoto(file))this.photo=file;
        else throw Error('请选择 DOCX、PDF、TXT 或图片');
      }catch(e){if(seq===this.fileSequence)this.singleError='文件读取失败：'+e.message}
      finally{if(seq===this.fileSequence)this.readingSingle=false}
    },
    selectWord(n){
      this.wordNumber=n;this.singleMatch=null;this.newAnswer=this.newAnalysis='';
      const row=this.singleWord;
      this.newType=['单选题','多选题','实验题','填空题','判断题','非选择题'].includes(row.type)?row.type:'单选题';
      if(this.singlePaper)this.singleNo=String(n);
    },
    async collectQuestion(choice=''){
      if(!this.singleComplete){this.singleError='请先补齐答案和解析。';return}
      if(this.readingSingle || (this.pdfDoc&&!this.photo))return;
      try{await this.ensureQuestions()}catch(error){this.singleError=error.message;return}
      const row=this.singleWord;
      const stem=row?(row.stem||row.plain.trim().split('\n')[0]||'图片题'):this.newStem.trim();
      if(!stem || (this.photoSourceId&&!this.appliedPhoto)){this.singleError='请先转写这道题的题面。';return}
      if(this.singlePaper&&this.qs.some(q=>q.src.some(s=>s[0]===this.singlePaper&&String(s[1])===this.singleNo.trim()))){this.singleError='这份卷已收录该题号，请核对。';return}
      const embedded=stem.match(/^\s*(?:\d+[.．]\s*)?(?:[（(]\d+分[)）]\s*)?[（(]((?:19|20)\d{2}\s*[·•・][^（）()]+)[）)]/);
      const question={id:selectionUUID(),manual:true,collectedAt:new Date().toLocaleDateString('sv-SE')+'T'+new Date().toTimeString().slice(0,8),stem,body:stem,k:[],src:this.singlePaper?[[this.singlePaper,this.singleNo.trim()]]:[],origin:this.newSource.trim()||embedded?.[1]?.trim()||'',type:this.newType,level:'',date:new Date().toLocaleDateString('sv-SE'),answer:this.newAnswer.trim(),a:this.newAnalysis.trim()};
      try{
        if(this.photoSourceId)question.photoSourceId=this.photoSourceId;
        if(row)Object.assign(question,{body:row.plain,contentHtml:row.content,answerHtml:hasSolution(row.answer)?row.answer:'',analysisHtml:hasSolution(row.analysis)?row.analysis:''});
        if(this.photo && this.appliedPhoto){
          question.manual=false;question.referencePhoto=this.appliedPhoto.picture;
          question.contentHtml='<p>'+escapeDoc(stem).replaceAll('\n','<br>')+'</p>';
          if(this.appliedPhoto.includeFigure)question.contentHtml+='<img alt="题目选区" src="'+this.appliedPhoto.picture+'">';
        }else if(this.photo){
          question.manual=false;question.contentHtml='<p>'+escapeDoc(stem)+'</p><img alt="题目配图" src="'+await imageData(this.photo)+'">';
        }
        const face=await duplicateFace(question);let match;
        for(const q of this.formalQuestions){if(await duplicateFace(q)===face){match=q;break}}
        if(match && this.singleMatch?.id!==match.id){this.singleMatch=match;return}
        if(match && choice==='link'){
          const record={id:question.id,questions:[],links:[{q:question,target:match.id}]};
          await accessRecords('readwrite',store=>store.add(record));this.attachRecord(record);
          this.$refs.collector.close();this.openDetail(match);return
        }
        if(row || this.photo || libraryServer){question.manual=false;await accessRecords('readwrite',store=>store.add({id:question.id,questions:[question]}))}
        this.qs.unshift(question);this.save()
      }catch(e){this.singleError='保存失败：'+e.message;return}
      this.clearFilters();this.order='new';this.pageNo=1;this.$refs.collector.close();this.openDetail(question);void this.classifyQuestions([question])
    },
    primaryType, setPage(n){this.pageNo=n;this.$nextTick(()=>this.$refs.filters?.scrollIntoView({block:'start'}))},
    photoPlaceholder(q){return /^[^\n]+\.(?:jpe?g|png|webp|gif|heic)$/i.test((q.stem||'').trim()) && (q.photoPlaceholder || !!q.contentHtml?.includes('<img'))},
    bankReady(q){return !this.photoPlaceholder(q) && !this.missing(q).length},
    hasField(q,f){
      if(f==='source')return Boolean(this.originOf(q));
      const values=f==='answer'?[q.answer,q.answerHtml]:[q.a,q.analysisHtml];
      return [this.fixes[q.id]?.[f],...values].some(v=>v && hasSolution(v));
    },
    missing(q){return ['answer','analysis'].filter(f=>!this.hasField(q,f))},
    fieldName(f){return {answer:'答案',analysis:'解析',source:'来源'}[f]},
    beginFix(f,ai){this.aiDraftToken++;this.aiBusy=false;this.aiDraftNotice='';this.editField=f;this.editValue=f==='source'?this.originOf(this.detail):'';this.helper='';
      if(!ai)return;
      const q=this.detail, answer=this.fixes[q.id]?.answer || q.answer || '', field=this.fieldName(f);
      const asText=html=>{const el=document.createElement('template');el.innerHTML=html||'';el.content.querySelectorAll('sup,sub').forEach(n=>n.replaceWith((n.tagName==='SUP'?'^':'_')+'('+n.textContent+')'));el.content.querySelectorAll('p,div,tr,br').forEach(n=>n.append('\n'));el.content.querySelectorAll('td,th').forEach(n=>n.append('　'));return el.content.textContent.trim()};
      this.helper=['请为下面这道高中生物'+q.type+'起草'+field+'，供教师审核。',f==='analysis'?'结合题干和选项说明判断依据。':'只给出参考答案，按原题顺序作答。','如缺图、条件不足或有歧义，请指出，不要猜测。','题面：',aiQuestionParts(q).text,f==='analysis' && (answer || q.answerHtml)?'已有参考答案：'+(answer||asText(q.answerHtml)):''].filter(Boolean).join('\n')
    },
    async copyAITask(){try{await navigator.clipboard.writeText(this.helper);this.notify('已复制题目和要求')}catch{this.$refs.aiTask.focus();this.$refs.aiTask.select();this.notify('已选中文字，请使用系统复制')}},
    acceptFix(){if(this.editField!=='source'&&!this.editValue.trim())return;this.fixes[this.detail.id]={...this.fixes[this.detail.id],[this.editField]:this.editValue.trim()};this.save();this.editField='';this.helper=''},
    addPurpose(){
      const name=this.newPurpose.trim();
      if(!name)return;
      if(['全部用途','未分类'].includes(name)){this.purposeError='请换一个用途名称。';return}
      if(!this.purposes.includes(name)){
        this.purposeNames.push(name);
        const oldExams=this.examPurposes;
        if(this.newExam&&!this.examPurposes.includes(name))this.examPurposes=[...this.examPurposes,name];
        try{this.save()}catch(error){this.examPurposes=oldExams;this.purposeNames.pop();this.purposeError=error.message;return}
      }
      this.purposeFilter=name;
      this.$refs.purposeDialog.close();
    },
    saveEdition(scope=null,grading=null){
      const purpose=this.paperPurpose.trim();
      if(['全部用途','未分类'].includes(purpose)){this.notify('请换一个用途名称。');return}
      if(!this.bag.length || !this.paperName.trim() || !purpose)return;
      if(this.chosen.length!==this.bag.length){this.notify('请等待已选题目载入后再保存');return}
      const kept=scope && new Set(scope.results.filter(r=>r.keep).map(r=>r.id));
      const selected=kept?this.chosen.filter(q=>kept.has(q.id)):this.chosen;
      if(!selected.length){this.notify('请至少保留一道题');return}
      if(selected.some(q=>!this.bankReady(q))){this.notify('试题篮含待补题，请补全或移出后再组卷。');return}
      if(this.examMode && selected.some(q=>q.type==='判断题')){
        this.notify('考试用卷不收判断题，请移出判断题或更改用途');return;
      }
      if(this.examMode && !grading){return this.beginGrading(selected,scope)}
      if(grading && JSON.stringify(selected.map(q=>[q.id,q.revision||0]))!==this.gradeIds){this.notify('选题已变化，请重新打开评分设置。');return}
      const item={id:selectionUUID(),isExam:this.examMode,purpose,name:this.paperName.trim(),date:new Date().toLocaleString('zh-CN'),ids:selected.map(q=>q.id),versions:Object.fromEntries(selected.map(q=>[q.id,q.revision||0])),marked:[],stats:[]};
      if(scope)item.scope=scope;
      if(grading)item.grading=grading;
      item.fixes=Object.fromEntries(item.ids.filter(id=>this.fixes[id]).map(id=>[id,{...this.fixes[id]}]));
      this.overviewItem=item;
      this.overviewSignature=this.basketSignature;
      if(grading && this.gradeHost)this.confirmOverview();
      else this.$nextTick(()=>this.$refs.paperOverview.showModal());
      return true;
    },
    confirmOverview(){
      const item=this.overviewItem;
      if(this.gradeHost && this.gradeEdition && item){
        this.editions.unshift(item);
        try{this.save()}catch(e){this.editions.shift();this.gradeError=e.message;this.$refs.paperOverview.close();return}
        this.$refs.paperOverview.close();this.overviewItem=null;this.closeGradeTab();this.gradeDraft=null;
        location.hash='review/'+item.id;return;
      }
      if(!item || this.overviewSignature!==this.basketSignature){this.notify('题目已变更，请返回重新核对。');if(this.gradeHost){this.gradeError=this.msg;this.$refs.paperOverview.close()}return}
      const draft=this.bag;
      this.editions.unshift(item);this.bag=[];
      try{this.save()}catch(error){this.editions.shift();this.bag=draft;this.notify(error.message);if(this.gradeHost){this.gradeError=error.message;this.$refs.paperOverview.close()}return}
      this.$refs.paperOverview.close();if(this.$refs.scopeDialog?.open)this.$refs.scopeDialog.close();
      if(this.gradeHost)this.closeGradeTab();this.gradeDraft=null;this.overviewItem=null;this.draftMarks=null;this.examOverride=null;
      this.$refs.basket?.close();this.paperName='';location.hash='review/'+item.id;
    },
    editionFace,
    changeEditionFormat(edition,event){
      edition.formatId=event.target.value;this.save();this.wordNotice={};
    },
    async uploadFormat(event){
      const file=event.target.files[0];event.target.value='';
      if(!file)return;
      this.formatBusy=true;this.formatMessage='';
      try{
        if(!/\.docx$/i.test(file.name))throw Error('请选择 .docx 格式的 Word 模板。');
        const bytes=await file.arrayBuffer();
        const id='format-'+await fileDigest(bytes);
        const existing=this.formats.find(f=>f.id===id);
        if(existing){this.formatMessage='模板库已有「'+existing.name+'」，可以直接选用。';return}
        const template=await prepareWordTemplate(bytes);
        const record={id,wordTemplate:true,name:file.name.replace(/\.docx$/i,''),...template};
        await accessRecords('readwrite',store=>store.put(record));
        this.attachRecord(record);
        this.formatMessage='已添加「'+record.name+'」。';
      }catch(error){this.formatMessage='模板未导入：'+error.message}finally{this.formatBusy=false}
    },
    async downloadEdition(edition,teacher){
      if(this.wordBusy)return;
      this.wordBusy=true;this.wordNotice={id:edition.id,text:'正在生成 Word…'};
      try{
        const items=edition.ids.map((id,i)=>{
          const q=this.editionQuestion(id,edition);
          if(q && this.conflictingLinks(q).length)throw Error('第 '+(i+1)+' 题存在答案冲突，请先核对。');
          if(!q)throw Error('第 '+(i+1)+' 题尚未载入，请稍后再试。');
          const fix=edition.fixes?.[id]||this.fixes[id]||{};
          const answer=q.answerHtml||plainBlocks(fix.answer||q.answer);
          const analysis=q.analysisHtml||plainBlocks(fix.analysis||q.a);
          if(!(q.answerHtml||fix.answer||q.answer)||!(q.analysisHtml||fix.analysis||q.a))throw Error('第 '+(i+1)+' 题缺少答案或解析，请补齐后再导出。');
          return {q,answer,analysis,source:fix.source??q.origin};
        });
        const template=edition.formatId?await accessRecords('readonly',store=>store.get(edition.formatId)):null;
        if(edition.formatId && !template)throw Error('找不到所选模板，请重新上传。');
        const blob=await makeEditionWord(edition,items,teacher,template);
        const url=URL.createObjectURL(blob),link=document.createElement('a');
        link.href=url;link.download=edition.name.replace(/[\\/:*?"<>|]/g,'_')+'（'+(teacher?'解析版':'学生版')+'）.docx';
        link.click();setTimeout(()=>URL.revokeObjectURL(url),30000);
        this.wordNotice={};
      }catch(error){this.wordNotice={id:edition.id,text:error.message}}finally{this.wordBusy=false}
    },
    markReview(id){
      const was=this.review.marked.includes(id);this.review.marked=this.flip(this.review.marked,id);
      if(!was&&!this.wrong.includes(id))this.wrong.push(id);this.save()
    },
    resetScores(){this.scoreBook=null;this.sheet='';this.sheets=[];this.scoreMessage='';this.scorePreview=[];this.scoreInput=''},
    async readScores(event){
      this.resetScores();const file=event.target.files[0];if(!file)return;
      try{
        if(/\.csv$/i.test(file.name)){
          this.scoreInput=(await file.text()).replace(/^\uFEFF/,'');this.previewScores();
        }else{
          await loadTool('xlsx');const book=XLSX.read(await file.arrayBuffer(),{type:'array'});this.scoreBook=Vue.markRaw(book);this.sheets=book.SheetNames;this.sheet=this.sheets[0];this.previewScores()
        }
      }catch{this.scoreMessage='文件读取失败，请检查格式，或粘贴表格。'}finally{event.target.value=''}
    },
    async previewScores(){
      this.scorePreview=[];this.scoreMessage='';
      try{
        await loadTool('xlsx');const book=this.scoreBook || XLSX.read(this.scoreInput.trim(),{type:'string',raw:true});
        const rows=XLSX.utils.sheet_to_json(book.Sheets[this.sheet||book.SheetNames[0]],{header:1,defval:''});
        this.scorePreview=readScoreRows(rows,this.review.ids.length).sort((a,b)=>b.missed-a.missed||a.n-b.n)
      }catch(e){this.scoreMessage=e.message}
    },
    applyScores(){this.review.stats=this.scorePreview.map(r=>({...r}));this.save();this.resetScores();this.scoreTab=false;this.notify('已按本卷题号保存匿名汇总')}
  }
};

const QuestionNav = {
  props: {ids: {type:Array, required:true}},
  data(){return {current:1, frame:0}},
  template: `<nav class="question-nav" aria-label="题号导航">
    <small>题号</small><div class="numbers" ref="numbers">
      <button v-for="(id,i) in ids" :key="id" :aria-label="'跳到第 '+(i+1)+' 题'"
        :aria-current="current===i+1?'location':null" @click="jump(i+1)">{{i+1}}</button>
    </div></nav>`,
  mounted(){
    window.addEventListener('scroll',this.queueUpdate,{passive:true});
    window.addEventListener('resize',this.queueUpdate);
    this.queueUpdate();
  },
  beforeUnmount(){
    window.removeEventListener('scroll',this.queueUpdate);
    window.removeEventListener('resize',this.queueUpdate);
    cancelAnimationFrame(this.frame);
  },
  methods:{
    jump(n){
      document.getElementById('review-question-'+n)?.scrollIntoView({block:'start'});
      this.queueUpdate();
    },
    queueUpdate(){
      if(this.frame)return;
      this.frame=requestAnimationFrame(()=>{
        this.frame=0;
        const boundary=Math.max(61,this.$el.getBoundingClientRect().bottom+2);
        let first=1,last=this.ids.length;
        while(first<last){
          const mid=Math.ceil((first+last)/2);
          const question=document.getElementById('review-question-'+mid);
          if(question && question.getBoundingClientRect().top<=boundary)first=mid;
          else last=mid-1;
        }
        if(this.current===first)return;
        this.current=first;
        const strip=this.$refs.numbers, button=strip.children[first-1];
        const bounds=strip.getBoundingClientRect(),target=button.getBoundingClientRect();
        if(target.right>bounds.right)strip.scrollLeft+=target.right-bounds.right;
        else if(target.left<bounds.left)strip.scrollLeft+=target.left-bounds.left;
      });
    }
  }
};
