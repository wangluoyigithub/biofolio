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
