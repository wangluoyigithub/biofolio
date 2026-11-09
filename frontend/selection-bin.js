// Recycle bin: deleted questions, imported papers and saved papers.
// "彻底删除" hides an item for good. The stored records stay on the server so
// saved papers that still use a question keep showing it and backups stay whole.
const recycleBin={
  data(){return {purgedQuestions:saved.purgedQuestions||[],purgedPapers:saved.purgedPapers||[],binTab:'题目',binOpen:''}},
  computed:{
    liveEditions(){return this.editions.filter(e=>!e.deletedAt)},
    binEditions(){return this.editions.filter(e=>e.deletedAt)},
    binPapers(){const gone=new Set(this.purgedPapers);return this.deletedPapers.filter(d=>!gone.has(d.id))},
    binQuestions(){const gone=new Set(this.purgedQuestions);return this.looseDeleted.filter(q=>!gone.has(q.id))},
    binCount(){return this.binQuestions.length+this.binPapers.length+this.binEditions.length}
  },
  methods:{
    paperQuestions(d){const byId=new Map(this.qs.map(q=>[q.id,q]));return (d.questions||[]).map(id=>byId.get(id)).filter(Boolean)},
    removeEdition(e){
      if(!confirm('把试卷「'+e.name+'」移到回收站？\n\n可在「回收站」恢复。'))return;
      const old=e.deletedAt;e.deletedAt=new Date().toLocaleDateString('sv-SE');
      if(this.persist(()=>{e.deletedAt=old}))this.notify('已移到回收站');
    },
    restoreEdition(e){
      const old=e.deletedAt;delete e.deletedAt;
      if(this.persist(()=>{e.deletedAt=old}))this.notify('已恢复试卷「'+e.name+'」');
    },
    purgeEdition(e){
      if(!confirm('彻底删除试卷「'+e.name+'」？\n\n删除后无法恢复。题库里的题目不受影响。'))return;
      const old=this.editions;this.editions=old.filter(x=>x!==e);
      if(this.persist(()=>{this.editions=old}))this.notify('已彻底删除');
    },
    purgeQuestion(q){
      if(!confirm('彻底删除这道题？\n\n'+(q.stem||'图片题').slice(0,80)+'\n\n删除后无法从回收站恢复。已组好的试卷仍保留这道题。'))return;
      const old=this.purgedQuestions;this.purgedQuestions=[...old,q.id];
      if(this.persist(()=>{this.purgedQuestions=old}))this.notify('已彻底删除');
    },
    purgePaper(d){
      if(!confirm('彻底删除试卷「'+this.paperLabel(d.id)+'」'+(d.questions?.length?'及其中 '+d.questions.length+' 道题':'')+'？\n\n删除后无法恢复，也不能再次导入同一文件。已组好的试卷仍保留原题。'))return;
      const old={papers:this.purgedPapers,questions:this.purgedQuestions};
      this.purgedPapers=[...old.papers,d.id];this.purgedQuestions=[...new Set([...old.questions,...(d.questions||[])])];
      if(this.persist(()=>Object.assign(this,{purgedPapers:old.papers,purgedQuestions:old.questions})))this.notify('已彻底删除');
    },
    restoreAll(){
      if(!this.binCount)return;
      if(!confirm('恢复回收站里的全部内容？'))return;
      const old={deleted:this.deletedQuestions,papers:this.deletedPapers,editions:this.editions};
      const keepQ=new Set(this.purgedQuestions),keepP=new Set(this.purgedPapers);
      this.deletedQuestions=old.deleted.filter(id=>keepQ.has(id));
      this.deletedPapers=old.papers.filter(d=>keepP.has(d.id));
      this.editions=old.editions.map(e=>{if(!e.deletedAt)return e;const copy={...e};delete copy.deletedAt;return copy});
      if(this.persist(()=>Object.assign(this,{deletedQuestions:old.deleted,deletedPapers:old.papers,editions:old.editions})))this.notify('回收站已全部恢复');
    },
    emptyBin(){
      if(!this.binCount)return;
      if(!confirm('清空回收站？\n\n'+this.binQuestions.length+' 道题目、'+this.binPapers.length+' 份导入试卷、'+this.binEditions.length+' 份已组试卷将被彻底删除，无法恢复。'))return;
      const old={questions:this.purgedQuestions,papers:this.purgedPapers,editions:this.editions};
      this.purgedPapers=[...old.papers,...this.binPapers.map(d=>d.id)];
      this.purgedQuestions=[...new Set([...old.questions,...this.binQuestions.map(q=>q.id),...this.binPapers.flatMap(d=>d.questions||[])])];
      this.editions=old.editions.filter(e=>!e.deletedAt);
      if(this.persist(()=>Object.assign(this,{purgedQuestions:old.questions,purgedPapers:old.papers,editions:old.editions})))this.notify('回收站已清空');
    }
  }
};
