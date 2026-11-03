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
      if(this.saveEdition(snapshot))this.$refs.scopeDialog.close();
      else this.scopeMessage=this.msg||'请检查试卷设置。';
    }
  }
};
