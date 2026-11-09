// Match the complete face, including option order, exponents, and the actual images.
async function duplicateFace(q) {
  const {text,images}=aiQuestionParts(q);
  const face=text.trim().replace(/^\d+\s*[.．、](?!\d)\s*/,'')
    .replace(/^[（(](?:19|20)\d{2}\s*[·•・][^（）()]+[)）]/,'').replace(/\s+/g,'');
  return JSON.stringify([face,await Promise.all(images.map(pictureKey))]);
}
// Looser comparison for questions reprinted with small edits: punctuation width,
// option style, a printed answer in the bracket, the source prefix and a few
// changed words no longer hide a duplicate. Teachers still confirm each match.
const DUP_THRESHOLD=0.85;
const dupWidth={'（':'(', '）':')', '［':'[', '］':']', '：':':', '；':';', '，':',', '。':'.', '．':'.', '？':'?', '！':'!', '“':'"', '”':'"', '‘':"'", '’':"'", '、':',', '　':' '};
function dupText(q){
  let t=aiQuestionParts(q).text.replace(/[（）［］：；，。．？！“”‘’、　]/g,c=>dupWidth[c]);
  t=t.replace(/^\s*(?:\d+|例\s*\d+)\s*[.,](?!\d)\s*/,'')
    .replace(/^\s*\((?:19|20)\d{2}\s*[·•・][^()]*\)/,'')
    .replace(/\(\s*(?:[A-H]{1,4}|[√×对错])?\s*\)/g,'()')
    .replace(/(^|[^A-Za-z])([A-H])\s*[.,]/g,'$1$2.')
    .replace(/_+/g,'_').replace(/\s+/g,'');
  return t;
}
function dupGrams(text){
  const grams=new Map();
  for(let i=0;i<text.length-1;i++){const g=text.slice(i,i+2);grams.set(g,(grams.get(g)||0)+1)}
  return {grams,size:Math.max(1,text.length-1),length:text.length};
}
// Dice coefficient over character pairs: 1 is identical, ~0.9 is a few words apart.
function dupScore(a,b){
  if(!a.length||!b.length)return 0;
  if(Math.min(a.length,b.length)/Math.max(a.length,b.length)<DUP_THRESHOLD*0.85)return 0;
  const [small,large]=a.grams.size<b.grams.size?[a,b]:[b,a];
  let shared=0;for(const [g,n] of small.grams)shared+=Math.min(n,large.grams.get(g)||0);
  return 2*shared/(a.size+b.size);
}
function answerText(q) {
  return q.answerHtml ? aiQuestionParts({contentHtml:q.answerHtml}).text.trim() : (q.answer||'').trim();
}
const duplicateTools={
  data(){return {linkedRows:[],singleMatch:null,singleMatchNote:'',dupPairs:[],dupBusy:false}},
  methods:{
    sourcesOf(q){return [...(q.src||[]),...this.linkedRows.filter(row=>row.target===q.id).flatMap(row=>row.q.src||[])]},
    linkedTo(q){return this.linkedRows.filter(row=>row.target===q.id)},
    conflictingLinks(q){
      if(!q)return [];
      const base=(this.fixes[q.id]?.answer||answerText(q)).replace(/\s/g,'');
      return this.linkedTo(q).filter(row=>{
        const other=answerText(row.q).replace(/\s/g,'');
        return base && other && base!==other && !(this.fixes[q.id]?.checkedLinks||[]).includes(row.q.id);
      });
    },
    dupIndex(){
      const key=this.formalQuestions.map(q=>q.id+'@'+(q.revision||0)).join(',');
      if(this._dupIndex?.key!==key)this._dupIndex={key,rows:this.formalQuestions.map(q=>({q,...dupGrams(dupText(q))})).filter(r=>r.length>8)};
      return this._dupIndex.rows;
    },
    // Best existing match for a new face, or null below the threshold.
    async findDuplicate(face,except=''){
      const probe=dupGrams(dupText(face));if(probe.length<=8)return null;
      let best=null;
      for(const row of this.dupIndex()){
        if(row.q.id===except)continue;
        const score=dupScore(probe,row);
        if(score>=DUP_THRESHOLD&&(!best||score>best.score))best={q:row.q,score};
      }
      if(best)best.exact=await duplicateFace(face)===await duplicateFace(best.q);
      return best;
    },
    dupNote(match){return match?(match.exact?'完全相同':'相似 '+Math.round(match.score*100)+'%'):''},
    matchSource(q){return this.sourcesOf(q).map(([p,n])=>this.paperLabel(p)+' 第'+n+'题').join('、') || q.origin || q.stem.slice(0,30)},
    async matchPaperRows(rows){
      for(const row of rows){
        const match=await this.findDuplicate({contentHtml:row.content});
        if(match){row.matchId=match.q.id;row.matchLabel=this.matchSource(match.q);row.matchNote=this.dupNote(match);row.link=false}
      }
    },
    // Library-wide scan for questions already collected twice.
    async scanDuplicates(){
      this.dupBusy=true;this.dupPairs=[];this.$refs.dupDialog.showModal();
      await new Promise(r=>setTimeout(r,30));
      const rows=this.dupIndex().slice().sort((a,b)=>a.length-b.length),pairs=[];
      for(let i=0;i<rows.length;i++){
        for(let j=i+1;j<rows.length;j++){
          if(rows[i].length/rows[j].length<DUP_THRESHOLD*0.85)break;
          const score=dupScore(rows[i],rows[j]);
          if(score>=DUP_THRESHOLD)pairs.push({a:rows[i].q,b:rows[j].q,score});
        }
        if(i%40===39)await new Promise(r=>setTimeout(r,0));
      }
      for(const p of pairs)p.exact=await duplicateFace(p.a)===await duplicateFace(p.b);
      this.dupPairs=pairs.sort((x,y)=>y.score-x.score);this.dupBusy=false;
    },
    dupRemove(pair,q){
      this.removeQuestion(q);
      if(this.deletedQuestions.includes(q.id))this.dupPairs=this.dupPairs.filter(p=>p.a.id!==q.id&&p.b.id!==q.id);
    }
  }
};
