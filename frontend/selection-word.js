// Turn the same saved question faces shown in the reader into editable Word content.
const plainBlocks = text => sciText(String(text || '')).split(/\r?\n/).map(line=>'<p>'+escapeDoc(line)+'</p>').join('');
// Drop the original paper's number (13. / 例4) from the start of a face.
function withoutPaperNumber(q,html){
  return String(html||'').replace(/^(<p>\s*)(?:(例\s*\d+)\s*[.．、]?|(\d+)\s*[.．、])[\s　]*/,(all,start,example,n)=>{
    const label=example?example.replace(/\s/g,''):n;
    return q.src?.some(ref=>String(ref[1]).replace(/\s/g,'')===label)?start:all;
  });
}
function editionFace(q){
  if(!q)return '';
  const html=q.contentHtml || plainBlocks(q.body && q.body!==q.stem && !q.body.startsWith(q.stem) ? q.stem+'\n'+q.body : q.body||q.stem);
  return tidyFace(q,withoutPaperNumber(q,html));
}
// One look for every face, on screen and in Word: no leading spaces or tabs,
// options written 「A．」, and no answer printed in a choice question's bracket.
// Only the displayed copy changes; the stored question keeps its original text.
const LEADING_SPACE=/^[\s\u3000\u00a0\u2002-\u200b\ufeff]+/;
const OPTION_START=/^([A-H])\s*[.．、]\s*/;
const OPTION_INLINE=/([\s\u3000]+)([B-H])\s*[.．、]\s*/g;
function wordLineStart(text){
  return text.replace(LEADING_SPACE,'').replace(OPTION_START,'$1．');
}
function tidyFace(q,html){
  if(!html)return html||'';
  const el=document.createElement('template');el.innerHTML=html;
  let lineStart=true,optionLine=false;
  const block=new Set(['P','DIV','LI','TD','TH','H1','H2','H3','H4']);
  (function visit(node){
    if(node.nodeType===Node.TEXT_NODE){
      let text=node.textContent;
      if(lineStart){text=text.replace(LEADING_SPACE,'');optionLine=OPTION_START.test(text);text=text.replace(OPTION_START,'$1．');if(text)lineStart=false}
      if(optionLine)text=text.replace(OPTION_INLINE,'$1$2．');
      node.textContent=text;return;
    }
    if(node.nodeName==='BR'){lineStart=true;optionLine=false;return}
    if(node.nodeName==='IMG'){lineStart=false;return}
    const isBlock=block.has(node.nodeName);
    if(isBlock){lineStart=true;optionLine=false}
    for(const child of Array.from(node.childNodes))visit(child);
    if(isBlock){lineStart=true;optionLine=false}
  })(el.content);
  let out=el.innerHTML;
  if(q && basketSection(q)<3)out=blankPrintedAnswer(out,shortAnswer(q.answerHtml||plainBlocks(q.answer)));
  return out;
}

async function wordImage(src){
  src=await inlinePicture(src);
  if(!src.startsWith('data:image/'))throw Error('配图无法读取');
  // img.decode() can stay pending in some embedded browsers; wait for load instead.
  const img=new Image();
  await new Promise((resolve,reject)=>{img.onload=resolve;img.onerror=()=>reject(Error('配图无法读取'));img.src=src;if(img.complete&&img.naturalWidth)resolve()});
  const scale=Math.min(1,590/img.naturalWidth,720/img.naturalHeight);
  let url=src, type=src.match(/^data:image\/(png|jpeg|gif|bmp);/i)?.[1];
  if(!type){
    const canvas=document.createElement('canvas');canvas.width=img.naturalWidth;canvas.height=img.naturalHeight;
    canvas.getContext('2d').drawImage(img,0,0);url=canvas.toDataURL('image/png');type='png';
  }
  return new docx.ImageRun({type:type==='jpeg'?'jpg':type,data:Uint8Array.from(atob(url.split(',')[1]),c=>c.charCodeAt(0)),transformation:{width:Math.round(scale*img.naturalWidth),height:Math.round(scale*img.naturalHeight)}});
}
async function wordBlocks(html, prefix=''){
  const root=document.createElement('template');root.innerHTML=html;
  const blocks=[];let runs=prefix?[new docx.TextRun(prefix)]:[];let lineStart=true;
  function flush(){if(runs.length){blocks.push(new docx.Paragraph({children:runs,spacing:{after:60},indent:{left:0,firstLine:0}}));runs=[]}lineStart=true}
  async function visit(node,style={}){
    if(node.nodeType===Node.TEXT_NODE){
      let text=node.textContent;
      if(lineStart){text=wordLineStart(text);if(!text)return;lineStart=false}
      runs.push(new docx.TextRun({...style,text}));return;
    }
    const tag=node.nodeName;
    if(tag==='IMG'){runs.push(await wordImage(node.getAttribute('src')));lineStart=false;return}
    if(tag==='BR'){runs.push(new docx.TextRun({break:1}));lineStart=true;return}
    if(tag==='TABLE'){
      flush();const rows=[];
      for(const row of node.rows){
        const cells=[];
        for(const cell of row.cells){
          const children=await wordBlocks(cell.innerHTML);
          cells.push(new docx.TableCell({children:children.length?children:[new docx.Paragraph('')]}));
        }
        rows.push(new docx.TableRow({children:cells}));
      }
      if(rows.length)blocks.push(new docx.Table({rows,width:{size:100,type:docx.WidthType.PERCENTAGE}}));return;
    }
    const props={SUP:{superScript:true},SUB:{subScript:true},STRONG:{bold:true},B:{bold:true},I:{italics:true},EM:{italics:true},U:{underline:{}}};
    for(const child of node.childNodes)await visit(child,{...style,...props[tag]});
    if(tag==='P'||tag==='DIV')flush();
  }
  for(const child of root.content.childNodes)await visit(child);
  flush();return blocks;
}
async function prepareWordTemplate(bytes){
  await loadTool('zip');
  const zip=await JSZip.loadAsync(bytes);
  const part='word/document.xml', entry=zip.file(part);
  if(!entry)throw Error('文件没有 Word 正文。');
  const xml=new DOMParser().parseFromString(await entry.async('string'),'application/xml');
  const ns='http://schemas.openxmlformats.org/wordprocessingml/2006/main';
  const body=xml.getElementsByTagNameNS(ns,'body')[0];
  if(!body)throw Error('无法读取 Word 正文，请另存为标准 .docx。');
  const paragraphs=Array.from(body.getElementsByTagNameNS(ns,'p'),p=>
    Array.from(p.getElementsByTagNameNS(ns,'t'),t=>t.textContent).join(''));
  const markers=paragraphs.filter(t=>t.includes('{{试题}}'));
  if(markers.length>1 || markers.some(t=>t.trim()!=='{{试题}}'))throw Error('请将 {{试题}} 单独放在正文的一个段落中。');
  if(!markers.length){
    const p=xml.createElementNS(ns,'w:p'),r=xml.createElementNS(ns,'w:r'),t=xml.createElementNS(ns,'w:t');
    t.textContent='{{试题}}';r.append(t);p.append(r);
    body.insertBefore(p,Array.from(body.children).find(n=>n.localName==='sectPr')||null);
    zip.file(part,new XMLSerializer().serializeToString(xml));
  }
  return {file:await zip.generateAsync({type:'blob',compression:'DEFLATE'}),hasTitle:paragraphs.some(t=>t.includes('{{试卷名称}}'))};
}
// 正文默认宋体小四号（12 磅），西文和数字用 Times New Roman。
const WORD_BODY_FONT={ascii:'Times New Roman',hAnsi:'Times New Roman',eastAsia:'宋体',cs:'Times New Roman'};
const WORD_PART_FONT={ascii:'Times New Roman',hAnsi:'Times New Roman',eastAsia:'楷体',cs:'Times New Roman'};
// Short choice answers (B / ABD / √) go into the answer table of the key.
function shortAnswer(html){
  const el=document.createElement('template');el.innerHTML=html||'';
  const text=el.content.textContent.replace(/^[\s\u3000]*(?:【?答案】?|答案[:：]?)[\s\u3000]*/,'').trim();
  const m=text.match(/^([A-H](?:[\s,，、]*[A-H])*|[√×✓✗对错]|正确|错误)(?=$|[\s。．.；;，,（(])/);
  return m?m[1].replace(/[\s,，、]/g,''):'';
}
async function answerKeyBlocks(items,grading,heading,part){
  const blocks=[heading('参考答案及评分标准',28)];
  const choice=[],rest=[];
  for(const [i,item] of items.entries()){
    const kind=basketSection(item.q),short=kind<3?shortAnswer(item.answer):'';
    (short?choice:rest).push({n:i+1,item,short});
  }
  if(choice.length){
    blocks.push(part('一、选择题'));
    const cell=(text,bold=false)=>new docx.TableCell({children:[new docx.Paragraph({alignment:docx.AlignmentType.CENTER,children:[new docx.TextRun({text:String(text),bold})]})]});
    for(let start=0;start<choice.length;start+=10){
      const row=choice.slice(start,start+10);
      const pad=Array(10-row.length).fill('');
      blocks.push(new docx.Table({width:{size:100,type:docx.WidthType.PERCENTAGE},rows:[
        new docx.TableRow({children:[cell('题号',true),...row.map(r=>cell(r.n,true)),...pad.map(()=>cell(''))]}),
        new docx.TableRow({children:[cell('答案',true),...row.map(r=>cell(r.short)),...pad.map(()=>cell(''))]})
      ]}));
      blocks.push(new docx.Paragraph({children:[],spacing:{after:60}}));
    }
    const kinds=new Set(choice.map(r=>basketSection(r.item.q)));
    if(grading)for(const kind of [1,2])if(kinds.has(kind)){
      const id=choice.find(r=>basketSection(r.item.q)===kind).item.q.id;
      blocks.push(...await wordBlocks(plainBlocks(markScheme(grading,id)),kind===1?'单项选择题：':'多项选择题：'));
    }
  }
  if(rest.length){
    blocks.push(part((choice.length?'二、':'')+'非选择题'));
    for(const {n,item} of rest){
      const rule=grading?.items[item.q.id];
      blocks.push(...await wordBlocks(item.answer,n+'．'+(rule?.kind===3?'（'+rule.points+'分）':'')));
      if(rule)blocks.push(...await wordBlocks(plainBlocks(markScheme(grading,item.q.id)+(rule.note?'\n'+rule.note:'')),'【评分细则】'));
    }
  }
  return blocks;
}
// Papers imported from a teacher's copy print the answer in the stem: 下列叙述错误的是（B）.
// Blank that bracket when it holds exactly this question's answer.
function blankPrintedAnswer(html,answer){
  if(!answer)return html;
  const letters=answer.split('').map(c=>c.replace(/[.*+?^${}()|[\]\\]/g,'\\$&')).join('[\\s,，、]*');
  const pattern=new RegExp('([（(])[\\s\\u3000]*'+letters+'[\\s\\u3000]*([）)])','g');
  const el=document.createElement('template');el.innerHTML=html;
  const walker=document.createTreeWalker(el.content,NodeFilter.SHOW_TEXT);
  for(let node=walker.nextNode();node;node=walker.nextNode())node.textContent=node.textContent.replace(pattern,'（　　）');
  return el.innerHTML;
}
// mode: 'student' | 'teacher' | 'answers'. A boolean still means teacher / student.
async function makeEditionWord(edition,items,mode,template=null){
  await loadTool('docx');
  if(typeof mode==='boolean')mode=mode?'teacher':'student';
  const teacher=mode==='teacher';
  const heading=(text,size)=>new docx.Paragraph({children:[new docx.TextRun({text,bold:true,size})],spacing:{before:160,after:120},keepNext:true});
  // 大题标题：楷体四号（14 磅）。
  const part=text=>new docx.Paragraph({children:[new docx.TextRun({text,bold:true,size:28,font:WORD_PART_FONT})],spacing:{before:200,after:120},keepNext:true});
  const blocks=template?.hasTitle?[]:[heading(edition.name,34)];
  const grading=edition.grading;
  if(grading)blocks.push(heading('满分 '+grading.total+' 分',24));
  if(mode==='answers')blocks.push(...await answerKeyBlocks(items,grading,heading,part));
  const headings=mode==='answers'?new Map():new Map(paperSections(items.map(item=>item.q),grading).map(group=>[group.questions[0].id,group.title]));
  for(const [i,item] of (mode==='answers'?[]:items).entries()){
    if(headings.has(item.q.id))blocks.push(part(headings.get(item.q.id)));
    const rule=grading?.items[item.q.id];
    const prefix=(i+1)+'．'+(rule?.kind===3?'（'+rule.points+'分）':'');
    blocks.push(...await wordBlocks(basketSection(item.q)<3?blankPrintedAnswer(scoredFace(item.q,grading),shortAnswer(item.answer)):scoredFace(item.q,grading),prefix));
    if(teacher){
      blocks.push(...await wordBlocks(item.answer,'【答案】'),...await wordBlocks(item.analysis,'【解析】'));
      if(rule)blocks.push(...await wordBlocks(plainBlocks(markScheme(grading,item.q.id)+(rule.note?'\n'+rule.note:'')),'【评分细则】'));
      if(item.source)blocks.push(...await wordBlocks(plainBlocks(item.source),'【来源】'));
    }
  }
  if(template){
    return docx.patchDocument({outputType:'blob',data:await template.file.arrayBuffer(),recursive:false,keepOriginalStyles:true,patches:{
      '试卷名称':{type:docx.PatchType.PARAGRAPH,children:[new docx.TextRun(edition.name)]},
      '试题':{type:docx.PatchType.DOCUMENT,children:blocks}
    }});
  }
  return docx.Packer.toBlob(new docx.Document({styles:{default:{document:{run:{font:WORD_BODY_FONT,size:24}}}},sections:[{properties:{page:{size:{width:11906,height:16838},margin:{top:1080,bottom:1080,left:1260,right:1260}}},children:blocks}]}));
}

// A ready-to-edit school paper: header, info line, notes, then the two markers.
async function makeSampleTemplate(){
  await loadTool('docx');
  const D=docx,C=D.AlignmentType.CENTER;
  const line=(text,opts={})=>new D.Paragraph({alignment:opts.align,spacing:{after:opts.after??80},children:[new D.TextRun({text,bold:opts.bold,size:opts.size,font:opts.font})]});
  return D.Packer.toBlob(new D.Document({styles:{default:{document:{run:{font:WORD_BODY_FONT,size:24}}}},sections:[{
    properties:{page:{size:{width:11906,height:16838},margin:{top:1080,bottom:1080,left:1260,right:1260}}},
    headers:{default:new D.Header({children:[line('××学校 2026—2027 学年第一学期',{align:C,size:21,after:0})]})},
    footers:{default:new D.Footer({children:[new D.Paragraph({alignment:C,children:[new D.TextRun({children:['生物试卷　第 ',D.PageNumber.CURRENT,' 页　共 ',D.PageNumber.TOTAL_PAGES,' 页'],size:18})]})]})},
    children:[
      line('{{试卷名称}}',{align:C,bold:true,size:32,after:120}),
      line('（考试时间：75 分钟　满分：100 分）',{align:C,size:21,after:160}),
      line('班级：__________　姓名：__________　考号：__________',{align:C,after:160}),
      line('注意事项：',{bold:true,size:21,after:40}),
      line('1．答题前，考生务必将自己的姓名、考号填写在答题卡上。',{size:21,after:40}),
      line('2．选择题答案用 2B 铅笔填涂在答题卡上；非选择题用黑色签字笔作答。',{size:21,after:160}),
      line('{{试题}}')
    ]}]}));
}
