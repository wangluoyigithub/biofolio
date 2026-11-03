// Points and marking notes belong to the edition, never to the collected original.
function initialBlankScores(q){
  const {text}=aiQuestionParts(q);
  const unit=Number(text.match(/每空\s*(\d+(?:\.5)?)\s*分/)?.[1] || 1);
  // Full-width underline-only paragraphs in imported papers are writing space.
  const lines=text.split(/\n/).filter(line=>!/^\s*[＿_]{2,}[。．.\s]*$/.test(line));
  const blanks=[...lines.join(' ').matchAll(/[_＿]{2,}(?:\s*[（(]\s*(\d+(?:\.5)?)\s*分\s*[）)])?/g)];
  return blanks.map(m=>m[1]?Number(m[1]):unit).join('，');
}
function parseBlankScores(raw){
  const values=String(raw).trim().split(/[，,、\s]+/);
  if(!values.every(v=>/^\d+(?:\.5)?$/.test(v) && Number(v)>0 && Number(v)<=999))return null;
  return values.map(Number);
}
function markScheme(g,id){
  const row=g?.items[id];if(!row)return '';
  if(row.kind===1)return '选对得 '+row.points+' 分，错选或未选得 0 分。';
  if(row.kind===2)return '全选对得 '+row.points+' 分，漏选且无错选得 '+g.partial+' 分，错选或未选得 0 分。';
  return '按作答顺序，各得分点依次为 '+row.parts.join('、')+' 分。';
}
function scoredFace(q,g){
  const face=editionFace(q);
  if(!g?.items[q.id])return face;
  const el=document.createElement('template');el.innerHTML=face;
  const walker=document.createTreeWalker(el.content,NodeFilter.SHOW_TEXT);
  // The new allocation is printed above the question; suppress original marks.
  while(walker.nextNode())walker.currentNode.nodeValue=walker.currentNode.nodeValue.replace(/[（(]\s*(?:共|每空|本题)?\s*\d+(?:\.\d+)?\s*分\s*[）)]/g,'');
  return el.innerHTML;
}
const examGrading={
  data(){return {gradeDraft:null,gradeError:'',gradeScope:null,gradeEdition:null,gradeIds:'',gradeAdvice:'',gradeBusy:false,gradeRun:0}},
  computed:{
    examMode(){return this.examPurposes.includes(this.paperPurpose.trim())},
    gradeSnapshot(){
      const d=this.gradeDraft;if(!d)return null;
      const items={};let total=0;
      const valid=v=>typeof v==='number' && v>0 && Number.isSafeInteger(v*2) && v<=999;
      for(const row of d.rows){
        const parts=row.kind===3?parseBlankScores(row.allocation):[];
        if(!parts)return null;
        const points=row.kind===3?parts.reduce((a,b)=>a+b,0):row.kind===1?d.single:d.multi;
        if(!valid(points))return null;
        if(row.kind===2 && (typeof d.partial!=='number'||!Number.isSafeInteger(d.partial*2)||d.partial<0||d.partial>=points))return null;
        items[row.id]={kind:row.kind,parts,points,note:row.note.trim()};total+=points;
      }
      return {single:d.single,multi:d.multi,partial:d.partial,target:d.target,total,items};
    }
  },
  methods:{
    markScheme,scoredFace,
    beginGrading(questions,scope=null,edition=null){
      if(!questions.length||questions.some(q=>!q)){this.notify('请等卷内题目载入后再设置评分');return}
      const prior=edition?.grading;
      this.gradeEdition=edition;this.gradeScope=scope;this.gradeIds=JSON.stringify(questions.map(q=>[q.id,q.revision||0]));
      this.gradeError=this.gradeAdvice='';this.gradeRun++;this.gradeBusy=false;
      this.gradeDraft={single:prior?.single??2,multi:prior?.multi??3,partial:prior?.partial??1,target:prior?.target??'',rows:questions.map(q=>{
        const kind=basketSection(q),old=prior?.items[q.id];
        const original=String(q.stem||'').match(/^\s*(?:\d+[．.]\s*)?[（(](\d+(?:\.\d+)?)\s*分[）)]/);
        return {id:q.id,kind,stem:q.stem,original:original?Number(original[1]):null,allocation:kind===3?(old?.parts.join('，')??initialBlankScores(q)):'',note:old?.note||''};
      })};
      this.$nextTick(()=>this.$refs.gradingDialog.showModal());
    },
    gradingQuestion(id){
      return this.gradeEdition?this.editionQuestion(id,this.gradeEdition):this.qs.find(q=>q.id===id);
    },
    async draftRule(row){
      const run=this.gradeRun;
      row.busy=true;row.proposal=null;row.error='';
      try{
        const q=this.gradingQuestion(row.id),fix=this.gradeEdition?.fixes?.[row.id]||this.fixes[row.id]||{};
        const face=aiQuestionParts(q);
        const answer=aiQuestionParts({contentHtml:q.answerHtml,stem:fix.answer||q.answer});
        const analysis=aiQuestionParts({contentHtml:q.analysisHtml,stem:fix.analysis||q.a});
        if(!answer.text && !answer.images.length)throw Error('请先补充参考答案，再起草评分细则。');
        const prompt='为高中生物试题起草可操作的教师评分细则。按作答顺序逐空列出小问、给分条件和分数，合理同义表述可给分。每空一般1分，有多个独立得分要点才建议加分。不得编造材料或答案，依据不足仅返回 {"error":"原因"}。只返回 JSON 对象 {"items":[{"rule":"第（1）问第一空：具体给分条件","points":1}]}，各项按空排列。每项分数须是0.5的正整数倍。当前各空拟分：'+row.allocation+'。题面：'+face.text+'。答案：'+answer.text+'。解析：'+analysis.text;
        const raw=await requestQuestionAI(this.aiConnection,prompt,[...face.images,...answer.images,...analysis.images]);
        if(run!==this.gradeRun)return;
        const data=JSON.parse(raw.trim().replace(/^```(?:json)?\s*|\s*```$/gi,''));
        if(data.error)throw Error(String(data.error));
        if(!Array.isArray(data.items)||!data.items.length||data.items.length>100||!data.items.every(x=>typeof x.rule==='string' && x.rule.trim() && typeof x.points==='number' && x.points>0 && x.points<=999 && Number.isSafeInteger(x.points*2)))throw Error('未得到可采用的逐空细则，请重试。');
        row.proposal={allocation:data.items.map(x=>x.points).join('，'),note:data.items.map(x=>x.rule+'（'+x.points+' 分）').join('\n')};
      }catch(e){if(run===this.gradeRun)row.error=e.message}finally{row.busy=false}
    },
    async suggestMarks(){
      const d=this.gradeDraft,g=this.gradeSnapshot,run=++this.gradeRun;
      this.gradeAdvice='';this.gradeBusy=true;
      try{
        const rows=d.rows.flatMap((r,i)=>{
          if(r.kind!==3)return [];
          const q=this.gradeEdition?this.editionQuestion(r.id,this.gradeEdition):this.qs.find(q=>q.id===r.id);
          const fix=(this.gradeEdition?.fixes||this.fixes)[r.id]||{};
          return [{题号:i+1,题面:aiQuestionParts(q).text,答案:fix.answer||q.answer||aiQuestionParts({contentHtml:q.answerHtml}).text,解析:fix.analysis||q.a||'',各空分值:r.allocation}];
        });
        const advice=rows.length?await requestQuestionAI(this.aiConnection,'根据题面及答案中的独立得分点提出简短评分建议。当前总分 '+g.total+'，目标 '+d.target+'。仅建议调整非选择题，指出第几题、第几个得分点和理由。材料不足请明确说明，无法合理调整则建议增减题目。\n'+JSON.stringify(rows)):'本卷没有可调整的大题，可调整选择题分值或增减题目。';
        if(run===this.gradeRun)this.gradeAdvice=advice;
      }catch(e){if(run===this.gradeRun)this.gradeAdvice=e.message}
      finally{if(run===this.gradeRun)this.gradeBusy=false}
    },
    finishGrading(){
      const g=this.gradeSnapshot;
      if(!g){this.gradeError='请核对分值，每道大题至少填写一个得分点。';return}
      if(g.target!=='' && g.target!==g.total){this.gradeError='合计与目标总分不符，请调整分值或清空目标。';return}
      if(this.gradeEdition){
        const copy={...JSON.parse(JSON.stringify(this.gradeEdition)),id:selectionUUID(),name:this.gradeEdition.name+'（评分版）',date:new Date().toLocaleString('zh-CN'),grading:g};
        this.editions.unshift(copy);
        try{this.save()}catch(e){this.editions.shift();this.gradeError=e.message;return}
        location.hash='review/'+copy.id;
      }else{
        if(!this.saveEdition(this.gradeScope,g)){this.gradeError=this.msg;return}
        if(this.$refs.scopeDialog?.open)this.$refs.scopeDialog.close();
      }
      this.$refs.gradingDialog.close();
    }
  }
};
