// Match the complete face, including option order, exponents, and the actual images.
async function duplicateFace(q) {
  const {text,images}=aiQuestionParts(q);
  const face=text.trim().replace(/^\d+\s*[.．、](?!\d)\s*/,'')
    .replace(/^[（(](?:19|20)\d{2}\s*[·•・][^（）()]+[)）]/,'').replace(/\s+/g,'');
  return JSON.stringify([face,await Promise.all(images.map(pictureKey))]);
}
function answerText(q) {
  return q.answerHtml ? aiQuestionParts({contentHtml:q.answerHtml}).text.trim() : (q.answer||'').trim();
}
const duplicateTools={
  data(){return {linkedRows:[],singleMatch:null}},
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
    async matchPaperRows(rows){
      const index=new Map();for(const q of this.formalQuestions)index.set(await duplicateFace(q),q);
      for(const row of rows){
        const match=index.get(await duplicateFace({contentHtml:row.content}));
        if(match){row.matchId=match.id;row.matchLabel=this.sourcesOf(match).map(([p,n])=>this.paperLabel(p)+' 第'+n+'题').join('、') || match.stem;row.link=false}
      }
    }
  }
};
