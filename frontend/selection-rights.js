// Who may change what. The administrator manages everything; other teachers
// edit, delete and restore only what they uploaded, and suggest changes to
// the rest. Purging the recycle bin is for the administrator only. The
// server checks the same rules (backend/library_rights.go).
const teacherRights={
  data(){return {paperOwners:{},suggestions:saved.suggestions||[],suggestText:'',suggestFor:null,suggestNotice:''}},
  computed:{
    canPurge(){return !libraryServer||this.admin},
    myOpenSuggestions(){return this.suggestions.filter(s=>this.canManage(s.q))}
  },
  methods:{
    ownerOf(q){const id=typeof q==='string'?q:q?.id;return (this.qs.find(row=>row.id===id)||q||{}).uploadedBy||''},
    // Ownership goes by account id; names can be changed and reused.
    ownerIdOf(q){const id=typeof q==='string'?q:q?.id;return (this.qs.find(row=>row.id===id)||q||{}).uploaderId||'admin'},
    canManage(q){
      if(!libraryServer||this.admin)return true;
      return !!this.accountId&&this.ownerIdOf(q)===this.accountId;
    },
    canManagePaper(id){
      if(!libraryServer||this.admin)return true;
      return !!this.accountId&&(this.paperOwners[id]||'admin')===this.accountId;
    },
    binManageable(x){return this.binTab==='题目'?this.canManage(x):this.binTab==='导入试卷'?this.canManagePaper(x.id):true},
    suggestionsFor(q){const id=typeof q==='string'?q:q?.id;return this.suggestions.filter(s=>s.q===id)},
    canClose(s){return this.canManage(s.q)||s.byId===this.accountId},
    startSuggest(q){this.suggestFor=q;this.suggestText='';this.suggestNotice='';this.$refs.suggest.showModal()},
    sendSuggestion(){
      const text=this.suggestText.trim();if(!text){this.suggestNotice='请写下需要修改的地方。';return}
      const old=this.suggestions,row={id:'s-'+Date.now().toString(36)+Math.random().toString(36).slice(2,6),q:this.suggestFor.id,by:this.accountUser,byId:this.accountId,text,at:new Date().toLocaleDateString('sv-SE')};
      this.suggestions=[...old,row];
      if(this.persist(()=>{this.suggestions=old})){this.$refs.suggest.close();this.notify('建议已提交，上传者和管理员会看到')}
    },
    closeSuggestion(s){
      const old=this.suggestions;this.suggestions=old.filter(x=>x.id!==s.id);
      if(this.persist(()=>{this.suggestions=old}))this.notify('已标记为处理完');
    }
  }
};
