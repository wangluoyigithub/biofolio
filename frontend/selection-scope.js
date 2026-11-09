// Scope review belongs to one paper. Nothing in this dialog changes the basket.
const scopeStatuses=['疑似超出','待核','未发现超出'];
function parseScopeAdvice(reply,text,allowed){
  const result=JSON.parse(reply.trim().replace(/^```(?:json)?\s*|\s*```$/gi,''));
  if(!scopeStatuses.includes(result.status) || !Array.isArray(result.findings))throw Error('未返回有效判断');
  const findings=result.findings.map(f=>{
    if(!['location','topic','reason'].every(k=>typeof f?.[k]==='string' && f[k].trim() && f[k].length<=500))throw Error('缺少判断依据');
    return {location:f.location,topic:f.topic,reason:f.reason};
  });
  if(result.status==='疑似超出' && (!findings.length || findings.some((f,i)=>
    allowed.includes(f.topic) || !result.findings[i].quote?.trim() ||
    !text.replace(/\s/g,'').includes(result.findings[i].quote.replace(/\s/g,'')))))throw Error('超范围依据不足');
  return {status:result.status,findings};
}
const scopeReview={
  data(){return {scopeTags:[],scopeRows:[],scopeRun:0,scopeBusy:false,scopeOpened:[],scopeSearch:'',scopeStep:'范围',scopeMessage:'',scopeSignature:''}},
  computed:{
    scopeTree(){
      const extra=this.topicRoots.find(root=>root.name==='其他已收录知识点');
      return extra?[...curriculumTree,extra]:curriculumTree;
    },
    scopeOptions(){return visibleTopicRows(this.scopeTree,this.scopeOpened,this.scopeSearch)},
    basketSignature(){return JSON.stringify(this.chosen.map(q=>[q.id,q.revision||0,this.knowledgeOf(q)]))}
  },
  methods:{
    openScope(){
      this.scopeStep='范围';this.scopeTags=[];this.scopeRows=[];this.scopeOpened=[];this.scopeSearch='';this.scopeMessage='';
      this.$refs.scopeDialog.showModal();
    },
    stopScope(){this.scopeRun++;this.scopeBusy=false},
    toggleScope(tags){
      const selected=new Set(this.scopeTags),remove=tags.every(t=>selected.has(t));
      for(const t of tags){if(remove)selected.delete(t);else selected.add(t)}
      this.scopeTags=[...selected];
    },
    async checkScope(){
      if(!this.scopeTags.length || !this.chosen.length)return;
      const run=++this.scopeRun;
      const allowed=[...this.scopeTags], connection=this.aiReady && this.aiConnection;
      this.scopeSignature=this.basketSignature;this.scopeStep='预览';this.scopeMessage='';
      this.scopeRows=this.chosen.map(q=>({id:q.id,keep:true,status:'待核',findings:[],note:connection?'等待检查':'请对照原题人工判断'}));
      this.scopeBusy=!!connection;
      if(!connection)return;
      for(const row of this.scopeRows){
        if(run!==this.scopeRun)break;
        const q=this.qs.find(q=>q.id===row.id);
        try{
          const {text,images}=aiQuestionParts(q);
          const tags=this.knowledgeOf(q);
          const instruction='检查本题作答必需的知识是否超出教师选定的已学范围。逐个看选择题选项或大题小问，共用图和材料一起读。背景术语、题面已给出的知识不能作为超范围的依据。区别同义概念。只判断本次进度，不判断是否超课标。图不清、材料不全、无法确定的部分列为待核。返回 JSON 对象，status 为疑似超出、待核或未发现超出。findings 为数组；每个疑点写 location（如 D项或第（2）问）、topic（必需知识）、quote（从题面逐字截取的对应短句）、reason（为什么必须用到或为何无法确定）。凡有确定的超范围位置归疑似超出，否则有不确定位置归待核。\n已学范围：'+allowed.join('、')+'\n整题初标仅供参考：'+tags.join('、')+'\n题面：\n'+text;
          const reply=await requestQuestionAI(connection,instruction,images);
          if(run!==this.scopeRun)break;
          Object.assign(row,parseScopeAdvice(reply,text,allowed),{note:''});
          if(!tags.length && row.status==='未发现超出')Object.assign(row,{status:'待核',note:'题目缺知识点标签，请人工核对。'});
        }catch(e){
          if(run!==this.scopeRun)break;
          row.note=e.message+'，请人工判断。';
          if(/连接失败|服务返回 (401|403|404|429)|请求超时/.test(e.message)){
            for(const pending of this.scopeRows.filter(r=>r.note==='等待检查'))pending.note='服务未完成检查，请人工判断。';
            break;
          }
        }
      }
      if(run===this.scopeRun)this.scopeBusy=false;
    },
    confirmScope(){
      if(this.scopeBusy || this.scopeSignature!==this.basketSignature){this.scopeMessage='试题篮已变化，请返回范围重新检查。';return}
      if(!this.paperName.trim()||!this.paperPurpose.trim()){this.scopeMessage='请填写试卷名称和用途。';return}
      const snapshot={topics:[...this.scopeTags],results:this.scopeRows.map(({id,status,keep,findings,note})=>({id,status,keep,findings,note}))};
      if(!this.saveEdition(snapshot) && !this.gradeDraft)this.scopeMessage=this.msg||'请检查试卷设置。';
    }
  }
};

// 一句话组卷: AI only turns the teacher's sentence into a range and type counts;
// questions are then drawn from the bank here and wait for the teacher to confirm.
const smartTypes=['判断题','单选题','多选题','非选择题'];
const smartCompose={
  data(){return {smartText:'',smartBusy:false,smartNotice:'',smartPlan:null,smartRun:0,smartAdded:[],smartPrevBag:null}},
  methods:{
    async smartRequest(){
      if(this.smartBusy){this.smartRun++;this.smartBusy=false;this.smartNotice='已停止。';return}
      const text=this.smartText.trim();
      if(!text){this.smartNotice='请先写一句组卷要求。';return}
      if(!this.aiReady){this.$refs.intakeAI.showModal();return}
      const run=++this.smartRun;this.smartBusy=true;this.smartNotice='AI 正在理解要求…';this.smartPlan=null;
      const sections=biologyBooks.map(([book,chapters])=>book+'：'+chapters.map(([chapter,...list],i)=>'第'+(i+1)+'章'+chapter+'（'+list.join('、')+'）').join('；')).join('\n');
      const topics=[...new Set(this.formalQuestions.flatMap(q=>this.knowledgeOf(q)))];
      const prompt=['把高中生物教师的一句组卷要求整理成 JSON，只返回 JSON。',
        '格式：{"sections":[],"topics":[],"counts":{"判断题":0,"单选题":0,"多选题":0,"非选择题":0},"avoidUsed":false,"name":"","purpose":"","note":""}',
        'sections 只能从下列人教版节名中选（说到某章就列出该章所有节）；topics 只能从题库已有知识点中选，没有提到具体知识点就留空数组。',
        'counts 是各题型题数；没说题数时按常见周测给出合理数量并在 note 说明。avoidUsed 表示是否避开以前组卷用过的题。name 和 purpose 没说就留空。',
        '教材目录：\n'+sections,'题库已有知识点：'+(topics.join('、')||'（无）'),'教师要求：'+text].join('\n');
      try{
        const reply=await requestQuestionAI(this.aiConnection,prompt);
        if(run!==this.smartRun)return;
        const data=JSON.parse(reply.trim().replace(/^\x60\x60\x60(?:json)?\s*/i,'').replace(/\s*\x60\x60\x60$/,''));
        const known=new Set(biologyBooks.flatMap(([,chapters])=>chapters.flatMap(([,...list])=>list)));
        const spec={
          sections:(Array.isArray(data.sections)?data.sections:[]).filter(s=>known.has(s)),
          topics:(Array.isArray(data.topics)?data.topics:[]).filter(t=>topics.includes(t)),
          counts:Object.fromEntries(smartTypes.map(t=>[t,Math.max(0,Math.min(60,Math.round(Number(data.counts?.[t])||0)))])),
          avoidUsed:data.avoidUsed===true,
          name:typeof data.name==='string'?data.name.trim().slice(0,60):'',
          purpose:typeof data.purpose==='string'?data.purpose.trim().slice(0,30):'',
          note:typeof data.note==='string'?data.note.trim().slice(0,200):''
        };
        if(!smartTypes.some(t=>spec.counts[t]))throw Error('没有读出题型和题数，请把要求写具体些，例如「10 道单选、2 道非选择题」。');
        this.smartPlan={spec,groups:[]};this.smartDraw();
        this.smartNotice='';this.smartPut();
      }catch(error){
        if(run===this.smartRun)this.smartNotice=error instanceof SyntaxError?'AI 返回的格式不对，请重试。':error.message;
      }finally{if(run===this.smartRun)this.smartBusy=false}
    },
    // Draw (or redraw) questions for the current plan; unused ones come first.
    smartDraw(){
      const plan=this.smartPlan;if(!plan)return;
      const {spec}=plan,used=new Set(this.editions.flatMap(e=>e.ids||[]));
      const inRange=q=>(!spec.sections.length&&!spec.topics.length) || this.sectionsOf(q).some(s=>spec.sections.includes(s)) || this.knowledgeOf(q).some(t=>spec.topics.includes(t));
      const pool=this.formalQuestions.filter(q=>inRange(q) && !(spec.avoidUsed&&used.has(q.id)));
      plan.groups=smartTypes.filter(t=>spec.counts[t]).map(type=>{
        const kind=smartTypes.indexOf(type);
        const list=pool.filter(q=>basketSection(q)===kind).map(q=>({q,key:(used.has(q.id)?1:0)+Math.random()})).sort((a,b)=>a.key-b.key).map(r=>r.q);
        return {type,want:spec.counts[type],ids:list.slice(0,spec.counts[type]).map(q=>q.id),available:list.length};
      });
    },
    // Drawn questions go straight into the side basket, where the teacher picks.
    smartPut(){
      const ids=this.smartPlan?.groups.flatMap(g=>g.ids)||[];
      if(!ids.length){this.smartNotice='范围内没有找到符合的题目。';return}
      const previous=this.bag;
      const replace=previous.length && confirm('试题篮里已有 '+previous.length+' 道题。\n\n确定：用这次抽到的 '+ids.length+' 道题替换。\n取消：追加在后面。');
      this.bag=replace?ids:[...previous,...ids.filter(id=>!previous.includes(id))];
      if(!this.persist(()=>{this.bag=previous}))return;
      this.smartAdded=ids.filter(id=>!previous.includes(id)||replace);this.smartPrevBag=previous;
      const {name,purpose}=this.smartPlan.spec;
      if(name&&!this.paperName.trim())this.paperName=name;
      if(purpose&&!this.paperPurpose.trim())this.paperPurpose=purpose;
      this.openBasket();
    },
    // Swap the drawn questions still in the basket for a fresh draw.
    smartRedraw(){
      if(!this.smartPlan)return;
      const previous=this.bag,drawn=new Set(this.smartAdded),kept=previous.filter(id=>!drawn.has(id));
      this.smartDraw();
      const ids=this.smartPlan.groups.flatMap(g=>g.ids).filter(id=>!kept.includes(id));
      this.bag=[...kept,...ids];
      if(!this.persist(()=>{this.bag=previous}))return;
      this.smartAdded=ids;this.openBasket();
    },
    smartUndo(){
      if(!this.smartPrevBag)return;
      const previous=this.bag;this.bag=this.smartPrevBag;
      if(!this.persist(()=>{this.bag=previous}))return;
      this.smartPlan=null;this.smartAdded=[];this.smartPrevBag=null;this.smartNotice='已撤销，试题篮恢复原样。';
    },
    smartQuestion(id){return this.qs.find(q=>q.id===id)},
    smartRangeText(spec){return [...spec.sections,...spec.topics].join('、')||'全部题库'}

  }
};

// AI 审卷: look for one item's answer showing up in another item's text, plus
// answer patterns and gaps. Findings are advice only; nothing is changed.
const leakPunct=/[\s　，。、；：:;,.!！?？“”"'‘’（）()【】\[\]《》<>…—\-_]/g;
function reviewOptions(text){
  const options={};
  for(const m of text.matchAll(/(?:^|\n|\s)([A-H])\s*[.．、]\s*([^\n]*?)(?=\s+[A-H]\s*[.．、]|\n|$)/g))if(!options[m[1]])options[m[1]]=m[2].trim();
  return options;
}
function sharedPhrase(key,other,min){
  let best='';
  for(let s=0;s+min<=key.length;s++){
    for(let len=Math.min(key.length-s,40);len>=Math.max(min,best.length+1);len--){
      if(other.includes(key.substr(s,len))){best=key.substr(s,len);break}
    }
  }
  return best;
}
// Split a question into what it asks: each （1）/(2)/①… part of a big question,
// or the whole stem for other questions. Used to spot two parts asking the same thing.
function reviewAsks(q,text){
  if(basketSection(q)<3)return [];
  const parts=[];let label='',buf=[];
  const flush=()=>{const t=buf.join('').trim();if(label&&t)parts.push({label,text:t});buf=[]};
  for(const line of text.split('\n')){
    const m=line.match(/^\s*([（(]\s*\d+\s*[)）]|[①②③④⑤⑥⑦⑧⑨⑩])\s*(.*)$/);
    if(m){flush();label=m[1].replace(/\s/g,'');buf=[m[2]]}else if(label)buf.push(line);
  }
  flush();
  return parts.map(p=>({...p,...dupGrams(p.text.replace(leakPunct,'').replace(/_+/g,'_'))})).filter(p=>p.length>=10);
}
const paperReview={
  data(){return {reviewBusy:false,reviewRun:0,reviewNotice:'',reviewReport:null}},
  computed:{reviewStale(){return !!this.reviewReport && this.reviewReport.signature!==this.basketSignature}},
  methods:{
    reviewAnswer(q){return String(this.fixes[q.id]?.answer||answerText(q)||'').trim()},
    // The text that gives each question's answer away.
    reviewKeys(q,text){
      const kind=basketSection(q),answer=this.reviewAnswer(q);
      if(kind===1||kind===2){const options=reviewOptions(text);return (answer.match(/[A-H]/g)||[]).map(l=>options[l]).filter(Boolean)}
      if(kind===0)return /^[√✓✔对]/.test(answer)?[text.split('\n')[0].replace(/[（(]\s*[)）]\s*$/,'')]:[];
      return answer.split(/[；;。\n]/).filter(s=>s.replace(leakPunct,'').length>=8);
    },
    localReview(){
      const items=this.chosen.map((q,i)=>{const text=aiQuestionParts(q).text;return {q,no:i+1,text,flat:text.replace(leakPunct,'')}});
      const leaks=[];
      for(const a of items)for(const key of this.reviewKeys(a.q,a.text)){
        const flat=key.replace(leakPunct,'');if(flat.length<6)continue;
        const min=Math.max(8,Math.ceil(flat.length*0.6));
        for(const b of items){
          if(b===a)continue;
          const phrase=sharedPhrase(flat,b.flat,Math.min(min,flat.length));
          if(phrase&&!leaks.some(l=>l.no===a.no&&l.other===b.no))leaks.push({no:a.no,other:b.no,phrase});
        }
      }
      // Same question twice, or two parts asking the same thing.
      const repeats=[];
      const faces=items.map(it=>({it,...dupGrams(dupText(it.q))}));
      for(let i=0;i<faces.length;i++)for(let j=i+1;j<faces.length;j++){
        const score=dupScore(faces[i],faces[j]);
        if(score>=DUP_THRESHOLD)repeats.push({no:faces[i].it.no,other:faces[j].it.no,a:'',b:'',score,whole:true});
      }
      const asks=items.flatMap(it=>reviewAsks(it.q,it.text).map(p=>({...p,no:it.no})));
      for(let i=0;i<asks.length;i++)for(let j=i+1;j<asks.length;j++){
        const a=asks[i],b=asks[j];
        if(repeats.some(r=>r.whole&&r.no===a.no&&r.other===b.no))continue;
        const score=dupScore(a,b);
        if(score>=0.8)repeats.push({no:a.no,other:b.no,a:a.label,b:b.label,score,phrase:b.text.slice(0,60)});
      }
      const notes=[];
      const choice=items.filter(it=>basketSection(it.q)===1).map(it=>({no:it.no,letter:(this.reviewAnswer(it.q).match(/[A-D]/)||[''])[0]})).filter(r=>r.letter);
      if(choice.length>=6){
        const counts=Object.fromEntries(['A','B','C','D'].map(l=>[l,choice.filter(r=>r.letter===l).length]));
        notes.push('单选答案分布：'+Object.entries(counts).map(([l,n])=>l+' '+n).join('，'));
        const top=Object.entries(counts).sort((x,y)=>y[1]-x[1])[0];
        if(top[1]>=Math.ceil(choice.length*0.4))notes.push('答案 '+top[0]+' 偏多（'+top[1]+'/'+choice.length+'），学生可能凭规律猜题。');
        for(const [l,n] of Object.entries(counts))if(!n&&choice.length>=8)notes.push('没有答案为 '+l+' 的单选题。');
        let run=1;for(let i=1;i<=choice.length;i++){if(i<choice.length&&choice[i].letter===choice[i-1].letter)run++;else{if(run>=4)notes.push('第 '+choice[i-run].no+'–'+choice[i-1].no+' 题答案连续都是 '+choice[i-1].letter+'。');run=1}}
      }
      const tags={};for(const it of items)for(const t of this.knowledgeOf(it.q))(tags[t]||=[]).push(it.no);
      for(const [t,nos] of Object.entries(tags))if(nos.length>=Math.max(4,Math.ceil(items.length*0.25)))notes.push('「'+t+'」考了 '+nos.length+' 道（第 '+nos.join('、')+' 题），可能重复考查。');
      const gaps=items.filter(it=>this.missing(it.q).length).map(it=>it.no);
      if(gaps.length)notes.push('第 '+gaps.join('、')+' 题缺答案或解析。');
      return {leaks,notes,items,repeats};
    },
    async reviewPaper(){
      if(this.reviewBusy){this.reviewRun++;this.reviewBusy=false;this.reviewNotice='已停止。';return}
      if(!this.chosen.length)return;
      const run=++this.reviewRun,local=this.localReview();
      this.reviewReport={signature:this.basketSignature,leaks:local.leaks,notes:local.notes,repeats:local.repeats,ai:null};
      if(!this.aiReady){this.reviewNotice='已完成本地检查。配置 AI 后可让 AI 进一步审查答案互相提示和科学性。';return}
      this.reviewBusy=true;this.reviewNotice='AI 正在审卷…';
      const lines=local.items.map(it=>'第'+it.no+'题【'+(it.q.type==='非选择题'?primaryType(it.q):it.q.type)+'】知识点：'+(this.knowledgeOf(it.q).join('、')||'未标注')+'\n题面：'+it.text.slice(0,basketSection(it.q)===3?2000:600)+'\n答案：'+(this.reviewAnswer(it.q)||'缺')).join('\n\n');
      const prompt=['你是高中生物命题审核员。审查下面这份试卷（'+(this.paperPurpose||'用途未填')+'，共 '+local.items.length+' 题），重点检查：',
        '1. 答案互相提示：某题的答案或关键信息，能否在其他题的题干、选项或材料中直接找到或推出。这是最重要的一项，请逐题对照。',
        '2. 设问重复：不同大题的小题之间、同一大题的各小题之间，是否在问同一个问题或要求写出同一个结论（措辞不同也算），例如两道题都问分泌蛋白的合成场所。请写明题号和小题号，如「第 18 题（2）与第 21 题（1）」。这一项也很重要，请逐个小题对照。',
        '3. 答案是否正确，题目有无科学性错误。','4. 题干表述是否有歧义。','5. 是否重复考查同一知识点，知识覆盖和难度梯度是否合理。',
        '题图没有发送，依赖图的内容若无法判断请说明，不要猜测。没有问题就返回空数组，不要编造。',
        '只返回 JSON：{"summary":"","issues":[{"no":0,"other":0,"kind":"答案提示|设问重复|答案疑误|科学性|表述歧义|重复考查|其他","detail":"","suggestion":""}]}。no 为题号，other 为相关的另一题题号，没有填 0。',
        lines].join('\n');
      try{
        const reply=await requestQuestionAI(this.aiConnection,prompt);
        if(run!==this.reviewRun)return;
        const data=JSON.parse(reply.trim().replace(/^\x60\x60\x60(?:json)?\s*/i,'').replace(/\s*\x60\x60\x60$/,''));
        const n=local.items.length,num=v=>{const x=Math.round(Number(v));return x>=1&&x<=n?x:0},str=v=>typeof v==='string'?sciText(v.trim()).slice(0,400):'';
        const issues=(Array.isArray(data.issues)?data.issues:[]).map(i=>({no:num(i?.no),other:num(i?.other),kind:str(i?.kind)||'其他',detail:str(i?.detail),suggestion:str(i?.suggestion)})).filter(i=>i.detail);
        const rank=k=>k==='答案提示'?0:k==='设问重复'?1:2;issues.sort((a,b)=>rank(a.kind)-rank(b.kind)||a.no-b.no);
        this.reviewReport.ai={summary:str(data.summary),issues};
        this.reviewNotice='';
      }catch(error){
        if(run===this.reviewRun)this.reviewNotice='AI 审卷未完成：'+(error instanceof SyntaxError?'返回格式不对，请重试。':error.message)+' 本地检查结果仍可参考。';
      }finally{if(run===this.reviewRun)this.reviewBusy=false}
    },
    reviewOpen(no){const q=this.chosen[no-1];if(q)this.openDetail(q)}
  }
};
