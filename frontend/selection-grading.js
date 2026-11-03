// Points and marking notes belong to the edition, never to the collected original.
// Keep text offsets so a blank split across Word runs still counts as one blank.
function questionLines(root){
  const lines=[];let text='',runs=[];
  function flush(){if(text.trim())lines.push({text,runs});text='';runs=[]}
  function read(node){
    if(node.nodeType===Node.TEXT_NODE){
      runs.push({node,start:text.length});text+=node.data;return;
    }
    const boundary=['P','DIV','BR','TD','TH','IMG'].includes(node.nodeName);
    if(boundary)flush();
    for(const child of node.childNodes)read(child);
    if(boundary)flush();
  }
  read(root);flush();return lines;
}
function blankLayout(q){
  const el=document.createElement('template');el.innerHTML=editionFace(q);
  const blanks=[],marks=[];let previous='',unit=1;
  for(const line of questionLines(el.content)){
    const text=line.text;
    const writingLine=/^\s*[_＿]{2,}[。．.\s]*$/.test(text);
    const continuation=writingLine && (!previous || /[_＿][。．.\s]*$/.test(previous) || (/^\s*＿{8,}/.test(text) && /[。！？]$/.test(previous)));
    for(const match of text.matchAll(/[（(]\s*(共|每空|本题)?\s*(\d+(?:\.\d+)?)\s*分\s*[）)]/g)){
      marks.push({line,start:match.index,end:match.index+match[0].length});
      if(match[1]==='每空')unit=Number(match[2]);
    }
    if(!continuation)for(const match of text.matchAll(/([_＿]{2,})(?:\s*[（(]\s*(\d+(?:\.\d+)?)\s*分\s*[）)])?/g)){
      blanks.push({line,end:match.index+match[1].length,original:Number(match[2])||null});
    }
    previous=text.trim();
  }
  return {el,blanks,marks,unit};
}
function initialBlankScores(q){
  const {blanks,unit}=blankLayout(q);
  return blanks.map(b=>b.original??unit).join('，');
}
function scoreRange(line,start,end){
  const locate=offset=>line.runs.findLast(run=>run.start<offset)||line.runs[0];
  const a=locate(start),b=locate(end),r=document.createRange();
  r.setStart(a.node,start-a.start);r.setEnd(b.node,end-b.start);
  return r;
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
  const rule=g?.items[q.id];
  if(!rule)return editionFace(q);
  const {el,marks,blanks}=blankLayout(q);
  const aligned=rule.kind===3 && blanks.length===rule.parts.length;
  // Build live ranges before editing, so DOM offsets follow earlier changes.
  const labels=aligned?blanks.map((b,i)=>({range:scoreRange(b.line,b.end,b.end),points:rule.parts[i]})):[];
  const oldMarks=marks.map(m=>scoreRange(m.line,m.start,m.end));
  for(const range of oldMarks.reverse())range.deleteContents();
  for(const {range,points} of labels.reverse()){
    if(points!==1)range.insertNode(document.createTextNode('（'+points+' 分）'));
  }
  if(rule.kind===3 && !aligned){
    const note=document.createElement('p');
    note.textContent='各得分点（按作答顺序）：'+rule.parts.join('、')+' 分';
    el.content.append(note);
  }
  return el.innerHTML;
}
function paperSections(items,grading){
  const groups=[];
  for(const q of items){
    const kind=basketSection(q);
    let group=groups.at(-1);
    if(!group||group.kind!==kind){group={kind,questions:[],points:0};groups.push(group)}
    group.questions.push(q);
    group.points+=grading?.items[q.id]?.points||0;
  }
  for(const [index,g] of groups.entries()){
    g.title='一二三四'[index]+'、'+basketSections[g.kind];
    if(!grading)continue;
    const rules={1:'每题 '+grading.single+' 分',2:'每题 '+grading.multi+' 分，漏选且无错选得 '+grading.partial+' 分，错选或未选得 0 分',3:'除特别注明外，每空 1 分'};
    g.title+='（'+rules[g.kind]+'，共 '+g.points+' 分）';
  }
  return groups;
}
// Build a preview from the current draft. Never use a model-provided total.
function readMarkProposal(raw, draft, snapshot) {
  let plan;
  try {
    plan = JSON.parse(raw.trim().replace(/^```(?:json)?\s*|\s*```$/gi, ''));
  } catch {
    throw Error('AI 没有返回可应用的分值，请重新生成。');
  }
  if (typeof plan?.reason !== 'string' || !plan.reason.trim()) throw Error('AI 未说明调分理由，请重新生成。');
  const changes = [];
  const counts = kind => draft.rows.filter(row => row.kind === kind).length;
  let total = snapshot.total;
  for (const [key, label, kind] of [['single','单选每题',1], ['multi','多选全对',2], ['partial','多选漏选',2]]) {
    const value = plan[key];
    if (value === undefined || value === snapshot[key]) continue;
    if (!counts(kind) || !Number.isSafeInteger(value) || typeof value !== 'number' ||
        value < (key === 'partial' ? 0 : 1) || value > 999) throw Error('选择题建议须为有效整分，请重新生成。');
    changes.push({key, label, before:snapshot[key], after:value});
    if (key !== 'partial') total += counts(kind) * (value - snapshot[key]);
  }
  if (counts(2) && (plan.partial ?? snapshot.partial) >= (plan.multi ?? snapshot.multi)) throw Error('漏选分必须低于多选满分，请重新生成。');
  const seen = new Set();
  if (plan.questions !== undefined && !Array.isArray(plan.questions)) throw Error('大题调分格式有误，请重新生成。');
  for (const item of plan.questions || []) {
    const number = item?.number;
    const row = draft.rows[number - 1];
    if (!Number.isInteger(number) || row?.kind !== 3 || seen.has(number)) throw Error('大题题号无效或重复，请重新生成。');
    seen.add(number);
    const before = snapshot.items[row.id].parts;
    const after = item.parts;
    if (!Array.isArray(after) || before.length !== after.length ||
        !after.every(n => typeof n === 'number' && n > 0 && Number.isSafeInteger(n)) ||
        after.reduce((a,b)=>a+b,0) > 999) throw Error('第 '+number+' 题的空数有误或分值不是有效整分，请重新生成。');
    if (before.every((n,i)=>n===after[i])) continue;
    changes.push({number, label:'第 '+number+' 题各空', before:before.join('、'), after:after.join('、'), parts:after});
    total += after.reduce((a,b)=>a+b,0) - snapshot.items[row.id].points;
  }
  return {reason:plan.reason, changes, total, base:JSON.stringify(snapshot)};
}
const examGrading={
  data(){return {gradeHost:null,gradeIndex:-1,examOverride:null,draftMarks:null,overviewItem:null,gradeDraft:null,gradeError:'',gradeScope:null,gradeEdition:null,gradeIds:'',gradeProposal:null,gradeFeedback:'',gradeRequests:[],gradeBusy:false,gradeRun:0}},
  computed:{
    gradeGroups(){return this.gradeSnapshot?paperSections(this.gradeDraft.rows.map(row=>this.gradingQuestion(row.id)),this.gradeSnapshot):[]},
    gradeRow(){return this.gradeDraft?.rows[this.gradeIndex]},
    gradeReading(){
      const row=this.gradeRow;if(!row)return null;
      const q=this.gradingQuestion(row.id),fix=(this.gradeEdition?.fixes||this.fixes)[row.id]||{};
      return {face:scoredFace(q,this.gradeSnapshot),
        answer:q.answerHtml||plainBlocks(fix.answer||q.answer||'待补答案'),
        analysis:q.analysisHtml||plainBlocks(fix.analysis||q.a||'待补解析')};
    },
    examMode:{get(){return this.examOverride??this.examPurposes.includes(this.paperPurpose.trim())},set(value){this.examOverride=value}},
    overviewSections(){return this.overviewItem?paperSections(this.overviewItem.ids.map(id=>this.editionQuestion(id,this.overviewItem)).filter(Boolean),this.overviewItem.grading):[]},
    reviewHeadings(){
      if(!this.review)return {};
      const questions=this.review.ids.map(id=>this.editionQuestion(id)).filter(Boolean);
      return Object.fromEntries(paperSections(questions,this.review.grading).map(group=>[group.questions[0].id,group.title]));
    },
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
    markScheme,scoredFace,parseBlankScores,
    beginGrading(questions,scope=null,edition=null){
      if(!questions.length||questions.some(q=>!q)){this.notify('请等卷内题目载入后再设置评分');return}
      const ids=JSON.stringify(questions.map(q=>[q.id,q.revision||0]));
      const same=this.gradeDraft && ids===this.gradeIds && this.gradeEdition?.id===edition?.id && JSON.stringify(scope)===JSON.stringify(this.gradeScope);
      if(this.gradeHost && !this.gradeHost.ownerDocument.defaultView.closed && same){
        this.gradeHost.ownerDocument.defaultView.focus();return true;
      }
      if(this.gradeHost)this.closeGradeTab();
      const tab=window.open('about:blank','_blank');
      if(!tab){this.notify('请允许此站点打开新标签页，再点击设置评分。');return false}
      const doc=tab.document;
      doc.documentElement.lang='zh-CN';
      doc.title='评分细则 · '+(edition?.name||this.paperName);
      for(const node of document.querySelectorAll('style,meta[name="viewport"]'))doc.head.append(node.cloneNode(true));
      const host=doc.createElement('div');doc.body.append(host);
      this.gradeHost=Vue.markRaw(host);
      const closeWithParent=()=>tab.close();
      window.addEventListener('pagehide',closeWithParent,{once:true});
      tab.addEventListener('pagehide',()=>{
        window.removeEventListener('pagehide',closeWithParent);
        if(this.gradeHost===host){this.gradeHost=null;this.gradeRun++;this.gradeBusy=false}
      },{once:true});
      this.gradeIndex=-1;
      if(same)return true;
      const prior=edition?edition.grading:this.draftMarks;
      this.gradeEdition=edition;this.gradeScope=scope;this.gradeIds=JSON.stringify(questions.map(q=>[q.id,q.revision||0]));
      this.gradeProposal=null;this.gradeError=this.gradeFeedback='';this.gradeRequests=[];this.gradeRun++;this.gradeBusy=false;
      this.gradeDraft={single:prior?.single??2,multi:prior?.multi??3,partial:prior?.partial??1,target:prior?.target??'',rows:questions.map(q=>{
        const kind=basketSection(q),old=prior?.items[q.id];
        const original=String(q.stem||'').match(/^\s*(?:\d+[．.]\s*)?[（(](\d+(?:\.\d+)?)\s*分[）)]/);
        return {id:q.id,kind,stem:q.stem,original:original?Number(original[1]):null,allocation:kind===3?(old?.parts.join('，')??initialBlankScores(q)):'',note:old?.note||''};
      })};
      return true;
    },
    closeGradeTab(){
      const tab=this.gradeHost?.ownerDocument.defaultView;
      this.gradeHost=null;this.gradeRun++;this.gradeBusy=false;
      if(tab && !tab.closed)tab.close();
    },
    selectGrade(index){
      this.gradeIndex=index;
      this.$nextTick(()=>{for(const pane of this.gradeHost?.querySelectorAll('.grade-pane')||[])pane.scrollTop=0});

    },
    gradingQuestion(id){
      return this.gradeEdition?this.editionQuestion(id,this.gradeEdition):this.qs.find(q=>q.id===id);
    },
    async draftRule(row){
      if(row.kind!==3)return;
      const run=this.gradeRun;
      const previous=row.proposal?.note||row.note;
      const feedback=(row.feedback||'').trim();
      row.busy=true;row.error='';
      try{
        const q=this.gradingQuestion(row.id),fix=this.gradeEdition?.fixes?.[row.id]||this.fixes[row.id]||{};
        const parts=parseBlankScores(row.allocation);
        if(!parts)throw Error('请先填好本卷各空分值，再起草细则。');
        if(!parts.every(Number.isInteger))throw Error('AI 细则按整分起草，请先把本卷各空分值改为整数。');
        const points=parts.reduce((sum,value)=>sum+value,0);
        const grading={items:{[q.id]:{kind:3,parts,points}}};
        const face=aiQuestionParts({contentHtml:scoredFace(q,grading)});
        const answer=aiQuestionParts({contentHtml:q.answerHtml,stem:fix.answer||q.answer});
        const analysis=aiQuestionParts({contentHtml:q.analysisHtml,stem:fix.analysis||q.a});
        if(!answer.text && !answer.images.length)throw Error('请先补充参考答案，再起草评分细则。');
        const prompt='本卷本题满分 '+points+' 分，各空分值依次为 '+parts.join('、')+' 分。分值已由教师确定，原卷及参考材料中的分数仅是历史标注。按本卷分值逐空说明答到什么才给分，部分给分也只用整数，不给半分。可说明合理同义表述。各空的分数由系统显示。按顺序只返回 JSON {"items":[{"rule":"给分条件"}]}，一个空一项。确实缺少作答依据时返回 {"error":"缺少什么依据"}。题面：'+face.text+'。答案：'+answer.text+'。解析：'+analysis.text;
        const raw=await requestQuestionAI(this.aiConnection,prompt+'\n当前草案与教师意见：'+JSON.stringify({当前草案:previous,此前意见:row.requests||[],本次意见:feedback}),[...face.images,...answer.images,...analysis.images]);
        if(run!==this.gradeRun)return;
        const data=JSON.parse(raw.trim().replace(/^```(?:json)?\s*|\s*```$/gi,''));
        if(data.error)throw Error(String(data.error));
        if(!Array.isArray(data.items)||data.items.length!==parts.length||
           !data.items.every((item,i)=>typeof item?.rule==='string' && item.rule.trim() &&
             (item.points===undefined || item.points===parts[i])))throw Error('AI 返回的细则与本卷各空分值不一致，请重新起草。');
        // Only match score expressions, so scientific quantities such as 0.5 mol remain valid.
        const fractionalMark=/(?:半\s*分|[点點][五伍]\s*分|(?:\d*\.\d*[1-9]\d*|\d+\s*\/\s*\d+)\s*分)/;
        if(data.items.some(item=>fractionalMark.test(item.rule.normalize('NFKC'))))throw Error('AI 细则中包含非整分，请按整分重新起草。');
        if(feedback)(row.requests||=[]).push(feedback);
        row.feedback='';
        const note=data.items.map((item,i)=>item.rule+'（'+parts[i]+' 分）').join('\n');
        row.proposal={base:parts.join(','),note};
        (row.dialogue||=[]).push({teacher:feedback||'起草本题细则',reply:note});
      }catch(e){if(run===this.gradeRun)row.error=e.message}finally{row.busy=false}
    },
    async suggestMarks(){
      if(this.gradeBusy || !this.gradeSnapshot)return;
      const d=this.gradeDraft,g=this.gradeSnapshot,run=++this.gradeRun;
      const feedback=this.gradeFeedback.trim();
      const requests=feedback?[...this.gradeRequests,feedback]:this.gradeRequests;
      const previous=this.gradeProposal;
      this.gradeError='';this.gradeBusy=true;
      try{
        const rows=d.rows.map((r,i)=>{
          const q=this.gradingQuestion(r.id),fix=(this.gradeEdition?.fixes||this.fixes)[r.id]||{};
          return {题号:i+1,题型:basketSections[r.kind],满分:g.items[r.id].points,题面:aiQuestionParts({contentHtml:scoredFace(q,g)}).text,答案:fix.answer||q.answer||aiQuestionParts({contentHtml:q.answerHtml}).text,解析:fix.analysis||q.a||aiQuestionParts({contentHtml:q.analysisHtml}).text,...(r.kind===3?{各空分值:r.allocation}:{})};
        });
        const sections=paperSections(d.rows.map(r=>this.gradingQuestion(r.id)),g).map(s=>s.title);
        const prompt='为高中生物教师建议怎样调整本卷分值。单选题、多选题和大题均可调整，只考虑卷中实际存在的题型。单选、多选各自统一每题分值，调整时按该类题数计算总分变化，不要单独调整某道选择题。多选漏选分必须小于每题满分，错选或未选得0分，改漏选分不改变卷面总分。大题可以针对具体空的独立得分点调分。所有建议分值只用整数，包括漏选分和各空分。每题每空满分至少1分。依据考查内容和各类题权重给出合理方案，列明改前、改后、理由和复算后的总分；不必每类都改。不能合理达到目标时说明差额并建议增减题，不编造题目。当前总分 '+g.total+'，目标 '+d.target+'。当前各类评分：'+sections.join('；')+'。卷内题目：\n'+JSON.stringify(rows);
        const format='只返回 JSON 对象。reason 是简短中文理由。只提供相对当前实际分值需要调整的字段：single 单选每题、multi 多选全对、partial 漏选（均为数字）；questions 数组每项为 {"number":本卷题号,"parts":[按原空序的全部新分值]}，不可改变空数。无法满足时在 reason 中说明。不使用 Markdown 或 LaTeX。';
        const context={历次教师意见:requests,上次建议:previous?{理由:previous.reason,调整:previous.changes}:null};
        const result=await requestQuestionAI(this.aiConnection,prompt+'\n'+format+'\n'+JSON.stringify(context));
        if(run!==this.gradeRun)return;
        if(JSON.stringify(g)!==JSON.stringify(this.gradeSnapshot))throw Error('评分已修改，请按当前分值重新生成。');
        this.gradeProposal=readMarkProposal(result,d,g);
        this.gradeRequests=requests;this.gradeFeedback='';
      }catch(e){if(run===this.gradeRun)this.gradeError=e.message}
      finally{if(run===this.gradeRun)this.gradeBusy=false}
    },
    applyMarkProposal(){
      const proposal=this.gradeProposal;
      if(!proposal || this.gradeBusy || !proposal.changes.length ||
          proposal.base!==JSON.stringify(this.gradeSnapshot))return;
      for(const change of proposal.changes){
        if(change.key)this.gradeDraft[change.key]=change.after;
        else this.gradeDraft.rows[change.number-1].allocation=change.parts.join('，');
      }
      this.gradeProposal=null;this.gradeRequests=[];this.gradeFeedback='';
      const gap=this.gradeDraft.target-proposal.total;
      this.gradeError=gap?'已应用，当前合计 '+proposal.total+' 分，'+(gap>0?'距目标还差 ':'超出目标 ')+Math.abs(gap)+' 分。':'已应用，已达到目标总分。';
    },
    finishGrading(){
      const g=this.gradeSnapshot;
      if(!g){this.gradeError='请核对分值，每道大题至少填写一个得分点。';return}
      if(g.target!=='' && g.target!==g.total){this.gradeError='合计与目标总分不符，请调整分值或清空目标。';return}
      if(this.gradeEdition){
        const copy={...JSON.parse(JSON.stringify(this.gradeEdition)),id:selectionUUID(),name:this.gradeEdition.name+'（评分版）',date:new Date().toLocaleString('zh-CN'),grading:g};
        this.overviewItem=copy;
        this.$nextTick(()=>this.$refs.paperOverview.showModal());
      }else{
        this.draftMarks=g;
        if(!this.saveEdition(this.gradeScope,g)){this.gradeError=this.msg;return}
      }

    }
  }
};
