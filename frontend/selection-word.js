// Turn the same saved question faces shown in the reader into editable Word content.
const plainBlocks = text => sciText(String(text || '')).split(/\r?\n/).map(line=>'<p>'+escapeDoc(line)+'</p>').join('');
// Drop the original paper's number (13. / 例4) from the start of a face.
function withoutPaperNumber(q,html){
  return String(html||'').replace(/^(<(?:p|div)>\s*)(?:(例\s*\d+)\s*[.．、]?|(\d+)\s*[.．、])[\s　]*/i,(all,start,example,n)=>{
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

async function wordImage(nodeOrSrc, isSubjective=false){
  const isEl = nodeOrSrc && typeof nodeOrSrc === 'object' && typeof nodeOrSrc.getAttribute === 'function';
  const src = isEl ? nodeOrSrc.getAttribute('src') : nodeOrSrc;
  const presetWMm = isEl ? Number(nodeOrSrc.getAttribute('data-w-mm')) : 0;
  const presetHMm = isEl ? Number(nodeOrSrc.getAttribute('data-h-mm')) : 0;
  const tier = (isEl && nodeOrSrc.getAttribute('data-tier')) || (isSubjective ? 'subjective' : 'choice');
  const manual = isEl && nodeOrSrc.getAttribute('data-manual-tier') === '1';
  const maxWidthMm = (isEl && Number(nodeOrSrc.getAttribute('data-max-w-mm'))) || undefined;
  const maxHeightMm = (isEl && Number(nodeOrSrc.getAttribute('data-max-h-mm'))) || undefined;
  const asset = await resolvePrintImageAsset(src, { maxWidthMm, maxHeightMm, isSubjective, tier, manual });
  const widthPx96 = presetWMm > 0 ? Math.max(1, Math.round(presetWMm * EMU_PER_MM) / EMU_PER_PX96) : asset.widthPx96;
  const heightPx96 = presetHMm > 0 ? Math.max(1, Math.round(presetHMm * EMU_PER_MM) / EMU_PER_PX96) : asset.heightPx96;
  return new docx.ImageRun({
    type: asset.type,
    data: asset.bytes,
    transformation: { width: widthPx96, height: heightPx96 }
  });
}
async function wordBlocks(html, prefix='', blockOpts={}){
  const compact = Boolean(blockOpts.compact);
  const isSubjective = Boolean(blockOpts.isSubjective);
  const afterSp = compact ? 18 : 60;
  const baseRun = compact ? { size: 24 } : {};
  const root=document.createElement('template');root.innerHTML=html;
  const rawSpecs=[];let runs=prefix?[new docx.TextRun({...baseRun,text:prefix})]:[];let lineStart=true;let paraHasImg=false;let paraAlign=blockOpts.align||undefined;
  function flush(){
    if(runs.length){
      const spacing = compact
        ? (paraHasImg
            ? { after: 14, line: 240, lineRule: docx.LineRuleType.AUTO }
            : { after: afterSp, line: 348, lineRule: docx.LineRuleType.EXACT })
        : { after: afterSp };
      rawSpecs.push({
        kind: 'p',
        hasImg: paraHasImg,
        opts: {
          children: runs,
          ...(paraAlign ? { alignment: paraAlign } : {}),
          spacing,
          ...(compact ? { snapToGrid: false, keepLines: true } : {}),
          indent: { left: 0, firstLine: 0 }
        }
      });
      runs=[];
    }
    lineStart=true;
    paraHasImg=false;
    paraAlign=blockOpts.align||undefined;
  }
  async function visit(node,style={}){
    if(node.nodeType===Node.TEXT_NODE){
      let text=node.textContent;
      if(lineStart){text=wordLineStart(text);if(!text)return;lineStart=false}
      runs.push(new docx.TextRun({...baseRun,...style,text}));return;
    }
    const tag=node.nodeName;
    if(tag==='IMG'){runs.push(await wordImage(node, isSubjective));paraHasImg=true;lineStart=false;return}
    if(tag==='BR'){runs.push(new docx.TextRun({break:1}));lineStart=true;return}
    if(tag==='TABLE'){
      flush();
      const noBdr={style:docx.BorderStyle.NONE,size:0,color:'auto'};
      const cellBdrs={top:noBdr,bottom:noBdr,left:noBdr,right:noBdr};
      const totalDxa=10206; // 180mm (210mm - 2 * 15mm)

      if(node.getAttribute('data-fig-wrap')==='1' && node.rows.length){
        const row = node.rows[0];
        const leftMm = Number(node.getAttribute('data-left-mm')) || 102;
        const leftDxa = Math.round((leftMm / 180) * totalDxa);
        const rightDxa = totalDxa - leftDxa;
        const keepInside = Boolean(blockOpts.keepQuestionIntact);
        const leftKids = row.cells[0] ? await wordBlocks(row.cells[0].innerHTML, '', { compact: true, isSubjective, keepAllNext: keepInside }) : [];
        const rightKids = row.cells[1] ? await wordBlocks(row.cells[1].innerHTML, '', { compact: true, isSubjective, align: docx.AlignmentType.RIGHT }) : [];
        rawSpecs.push({
          kind: 'table',
          hasImg: true,
          node: new docx.Table({
            layout: docx.TableLayoutType.FIXED,
            width: { size: totalDxa, type: docx.WidthType.DXA },
            columnWidths: [leftDxa, rightDxa],
            borders: { top: noBdr, bottom: noBdr, left: noBdr, right: noBdr, insideHorizontal: noBdr, insideVertical: noBdr },
            rows: [new docx.TableRow({
              cantSplit: true,
              children: [
                new docx.TableCell({
                  width: { size: leftDxa, type: docx.WidthType.DXA },
                  borders: cellBdrs,
                  margins: { top: 0, bottom: 0, left: 0, right: 113 },
                  verticalAlign: docx.VerticalAlign.TOP,
                  children: leftKids.length ? leftKids : [new docx.Paragraph({ snapToGrid: false, spacing: { after: 0, line: 240 }, children: [] })]
                }),
                new docx.TableCell({
                  width: { size: rightDxa, type: docx.WidthType.DXA },
                  borders: cellBdrs,
                  margins: { top: 0, bottom: 0, left: 40, right: 0 },
                  verticalAlign: docx.VerticalAlign.CENTER,
                  children: rightKids.length ? rightKids : [new docx.Paragraph({ snapToGrid: false, spacing: { after: 0, line: 240 }, children: [] })]
                })
              ]
            })]
          })
        });
        return;
      }

      if(node.getAttribute('data-fig-pair')==='2' && node.rows.length){
        const colW = Math.floor(totalDxa / 2);
        const cells = [];
        for (const cell of node.rows[0].cells) {
          const kids = await wordBlocks('<p>' + cell.innerHTML + '</p>', '', { compact: true, isSubjective, align: docx.AlignmentType.CENTER });
          cells.push(new docx.TableCell({
            width: { size: colW, type: docx.WidthType.DXA },
            borders: cellBdrs,
            margins: { top: 20, bottom: 20, left: 40, right: 40 },
            verticalAlign: docx.VerticalAlign.CENTER,
            children: kids.length ? kids : [new docx.Paragraph({ snapToGrid: false, spacing: { after: 0, line: 240 }, children: [] })]
          }));
        }
        rawSpecs.push({
          kind: 'table',
          hasImg: true,
          node: new docx.Table({
            layout: docx.TableLayoutType.FIXED,
            width: { size: colW * 2, type: docx.WidthType.DXA },
            columnWidths: [colW, colW],
            borders: { top: noBdr, bottom: noBdr, left: noBdr, right: noBdr, insideHorizontal: noBdr, insideVertical: noBdr },
            rows: [new docx.TableRow({ cantSplit: true, children: cells })]
          })
        });
        return;
      }

      const optCols=Number(node.getAttribute('data-opt-grid'))||0;
      if(optCols>0){
        const colW=Math.floor(totalDxa/optCols);
        const colWidths=Array(optCols).fill(colW);
        const rows=[];
        const rCount=node.rows.length;
        for(let ri=0; ri<rCount; ri++){
          const row=node.rows[ri];
          const keepRowNext = Boolean(blockOpts.keepQuestionIntact) && ri < rCount - 1;
          const cells=[];
          for(const cell of row.cells){
            const children=await wordBlocks('<p>'+cell.innerHTML+'</p>','',{compact:true,keepAllNext:keepRowNext});
            cells.push(new docx.TableCell({
              width:{size:colW,type:docx.WidthType.DXA},
              borders:cellBdrs,
              margins:{top:6,bottom:6,left:0,right:60},
              children:children.length?children:[new docx.Paragraph({snapToGrid:false,spacing:{after:12,line:280},children:[]})]
            }));
          }
          rows.push(new docx.TableRow({cantSplit:true,children:cells}));
        }
        if(rows.length){
          rawSpecs.push({
            kind: 'table',
            hasImg: false,
            node: new docx.Table({
              layout:docx.TableLayoutType.FIXED,
              width:{size:colW*optCols,type:docx.WidthType.DXA},
              columnWidths:colWidths,
              borders:{top:noBdr,bottom:noBdr,left:noBdr,right:noBdr,insideHorizontal:noBdr,insideVertical:noBdr},
              rows
            })
          });
        }
        return;
      }
      const rows=[];
      for(const row of node.rows){
        const cells=[];
        for(const cell of row.cells){
          const children=await wordBlocks(cell.innerHTML,'',blockOpts);
          cells.push(new docx.TableCell({children:children.length?children:[new docx.Paragraph('')],columnSpan:Number(cell.getAttribute('colspan'))||undefined,rowSpan:Number(cell.getAttribute('rowspan'))||undefined}));
        }
        rows.push(new docx.TableRow({children:cells}));
      }
      if(rows.length)rawSpecs.push({kind:'table',hasImg:false,node:new docx.Table({rows,width:{size:100,type:docx.WidthType.PERCENTAGE}})});return;
    }
    if((tag==='P'||tag==='DIV') && node.getAttribute?.('data-fig-center')==='1'){
      paraAlign = docx.AlignmentType.CENTER;
    }
    const props={SUP:{superScript:true},SUB:{subScript:true},STRONG:{bold:true},B:{bold:true},I:{italics:true},EM:{italics:true},U:{underline:{}}};
    for(const child of node.childNodes)await visit(child,{...style,...props[tag]});
    if(tag==='P'||tag==='DIV')flush();
  }
  for(const child of root.content.childNodes)await visit(child);
  flush();

  // Apply Carbon pagination lock policy:
  // - Objective question (keepQuestionIntact): keepNext on all paragraphs except the final block
  // - Subjective question (bindStemToMedia): bind ONLY stem paragraph (index 0) to an immediately following figure/media block
  const out=[];
  for(let idx=0; idx<rawSpecs.length; idx++){
    const sp = rawSpecs[idx];
    if(sp.kind === 'table'){ out.push(sp.node); continue; }
    let keepNext = false;
    if(blockOpts.keepAllNext && idx < rawSpecs.length - 1) keepNext = true;
    else if(blockOpts.keepQuestionIntact && idx < rawSpecs.length - 1) keepNext = true;
    else if(blockOpts.bindStemToMedia && idx === 0 && rawSpecs[1]?.hasImg) keepNext = true;
    out.push(new docx.Paragraph({ ...sp.opts, ...(keepNext ? { keepNext: true } : {}) }));
  }
  return out;
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
  const boxMarkers=paragraphs.filter(t=>t.includes('{{学号方框}}'));
  if(boxMarkers.length>1 || boxMarkers.some(t=>t.trim()!=='{{学号方框}}'))throw Error('请将 {{学号方框}} 单独放在正文的一个段落中。');
  return {file:await zip.generateAsync({type:'blob',compression:'DEFLATE'}),hasTitle:paragraphs.some(t=>t.includes('{{试卷名称}}')),hasStudentBox:boxMarkers.length>0};
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
async function makeEditionWord(edition,items,mode,template=null,gradingOpts=null){
  await loadTool('docx');
  if(typeof mode==='boolean')mode=mode?'teacher':'student';
  const teacher=mode==='teacher';
  const heading=(text,size)=>new docx.Paragraph({children:[new docx.TextRun({text,bold:true,size})],spacing:{before:160,after:120},keepNext:true});
  // 大题标题：楷体四号（14 磅）。
  const part=text=>new docx.Paragraph({children:[new docx.TextRun({text,bold:true,size:28,font:WORD_PART_FONT})],spacing:{before:200,after:120},keepNext:true});
  const gradingMode=Boolean(gradingOpts?.enabled);
  const studentGrading=gradingMode && mode==='student';
  const secPart=text=>new docx.Paragraph({children:[new docx.TextRun({text,bold:true,size:studentGrading?24:28,font:WORD_PART_FONT})],spacing:{before:studentGrading?80:200,after:studentGrading?40:120,...(studentGrading?{line:260}:{})},...(studentGrading?{snapToGrid:false}:{}),keepNext:true});
  const effectiveTemplate=template || (gradingMode ? {file:await makeGradingBaseTemplate(gradingOpts.schoolHeader,studentGrading),hasTitle:true,hasStudentBox:studentGrading} : null);
  let hasStudentBox=Boolean(effectiveTemplate?.hasStudentBox);
  if(effectiveTemplate && effectiveTemplate.hasStudentBox===undefined){
    try{
      await loadTool('zip');
      const z=await JSZip.loadAsync(await effectiveTemplate.file.arrayBuffer());
      const docText=await z.file('word/document.xml')?.async('string');
      hasStudentBox=Boolean(docText && docText.includes('{{学号方框}}'));
    }catch{hasStudentBox=false}
  }
  const blocks=effectiveTemplate?.hasTitle?[]:[heading(edition.name,studentGrading?30:34)];
  const grading=edition.grading;
  const studentMetaPara = (studentGrading && grading)
    ? new docx.Paragraph({alignment:docx.AlignmentType.CENTER,snapToGrid:false,spacing:{before:0,after:40,line:260},children:[new docx.TextRun({text:[edition.purpose,'满分 '+grading.total+' 分',items.length+' 道题'].filter(Boolean).join('　·　'),size:18,color:'444444'})]})
    : null;
  if(grading && !studentGrading){
    blocks.push(heading('满分 '+grading.total+' 分',24));
  }
  if(studentGrading && !hasStudentBox){
    if(studentMetaPara)blocks.push(studentMetaPara);
    blocks.push(...makeStudentIdBoxWordBlocks(gradingOpts.studentIdDigits));
  }
  if(mode==='answers')blocks.push(...await answerKeyBlocks(items,grading,heading,secPart));
  const choiceEntries=studentGrading?collectChoiceBoxEntries(edition,items):[];
  const lastChoiceIdx=choiceEntries.length?choiceEntries[choiceEntries.length-1].qIndex:-1;
  const headings=mode==='answers'?new Map():new Map(paperSections(items.map(item=>item.q),grading).map(group=>[group.questions[0].id,group.title]));
  for(const [i,item] of (mode==='answers'?[]:items).entries()){
    if(headings.has(item.q.id))blocks.push(secPart(headings.get(item.q.id)));
    const rule=grading?.items[item.q.id];
    const qKind=rule?rule.kind:basketSection(item.q);
    const isChoice=qKind<3;
    const prefix=(i+1)+'．'+(qKind===3&&rule?'（'+rule.points+'分）':'');
    const baseHtml=isChoice
      ? blankPrintedAnswer(scoredFace(item.q,grading),shortAnswer(item.answer))
      : scoredFace(item.q,grading);
    const {tier:qTier,manual:qManual}=questionImageTier(edition,item.q,!isChoice);
    const qHtml=studentGrading
      ? layoutQuestionFiguresHtml(await inlineGradingPictures(baseHtml,{isSubjective:!isChoice,tier:qTier,manualTier:qManual}),{bodyWidthMm:GRADING_BODY_WIDTH_MM,isChoice,tier:qTier,manualTier:qManual})
      : await inlineGradingPictures(baseHtml,{isSubjective:!isChoice,tier:qTier,manualTier:qManual});
    blocks.push(...await wordBlocks(qHtml,prefix,{
      compact:studentGrading,
      isSubjective:!isChoice,
      keepQuestionIntact:studentGrading&&isChoice,
      bindStemToMedia:studentGrading&&!isChoice
    }));
    if(teacher){
      blocks.push(...await wordBlocks(item.answer,'【答案】'),...await wordBlocks(item.analysis,'【解析】'));
      if(rule)blocks.push(...await wordBlocks(plainBlocks(markScheme(grading,item.q.id)+(rule.note?'\n'+rule.note:'')),'【评分细则】'));
      if(item.source)blocks.push(...await wordBlocks(plainBlocks(item.source),'【来源】'));
    }
    if(studentGrading && i===lastChoiceIdx && choiceEntries.length){
      blocks.push(...makeChoiceAnswerBoxWordBlocks(choiceEntries));
    }
  }
  if(effectiveTemplate){
    const patches={
      '试卷名称':{type:docx.PatchType.PARAGRAPH,children:[new docx.TextRun(edition.name)]},
      '试题':{type:docx.PatchType.DOCUMENT,children:blocks}
    };
    if(hasStudentBox){
      patches['学号方框']=studentGrading
        ? {type:docx.PatchType.DOCUMENT,children:[...(studentMetaPara?[studentMetaPara]:[]),...makeStudentIdBoxWordBlocks(gradingOpts.studentIdDigits)]}
        : {type:docx.PatchType.PARAGRAPH,children:[]};
    }
    const patched=await docx.patchDocument({outputType:'blob',data:await effectiveTemplate.file.arrayBuffer(),recursive:false,keepOriginalStyles:true,patches});
    if(studentGrading){
      const templateHeader=template?await extractTemplateHeaderText(template.file):'';
      return configureSingleSectionGradingWord(patched,{
        paperId:gradingOpts.paperId,
        title:edition.name,
        schoolHeader:templateHeader || gradingOpts.schoolHeader || ''
      });
    }
    return patched;
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
