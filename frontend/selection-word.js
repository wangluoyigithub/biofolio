// Turn the same saved question faces shown in the reader into editable Word content.
const plainBlocks = text => String(text || '').split(/\r?\n/).map(line=>'<p>'+escapeDoc(line)+'</p>').join('');
function editionFace(q){
  if(!q)return '';
  const html=q.contentHtml || plainBlocks(q.body && q.body!==q.stem && !q.body.startsWith(q.stem) ? q.stem+'\n'+q.body : q.body||q.stem);
  return html.replace(/^(<p>\s*)(\d+)[.．、](?!\d)/,(all,start,n)=>q.src?.some(ref=>String(ref[1])===n)?start:all);
}

async function wordImage(src){
  if(!src.startsWith('data:image/'))throw Error('配图无法读取');
  const img=new Image();img.src=src;await img.decode();
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
  const blocks=[];let runs=prefix?[new docx.TextRun(prefix)]:[];
  function flush(){if(runs.length){blocks.push(new docx.Paragraph({children:runs,spacing:{after:60}}));runs=[]}}
  async function visit(node,style={}){
    if(node.nodeType===Node.TEXT_NODE){runs.push(new docx.TextRun({...style,text:node.textContent}));return}
    const tag=node.nodeName;
    if(tag==='IMG'){runs.push(await wordImage(node.getAttribute('src')));return}
    if(tag==='BR'){runs.push(new docx.TextRun({break:1}));return}
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
async function makeEditionWord(edition,items,teacher,template=null){
  const heading=(text,size)=>new docx.Paragraph({children:[new docx.TextRun({text,bold:true,size})],spacing:{before:160,after:120},keepNext:true});
  const blocks=template?.hasTitle?[]:[heading(edition.name,34)];
  let section=-1,number=0;
  for(const [i,item] of items.entries()){
    const group=basketSection(item.q);
    if(section!==group){section=group;blocks.push(heading('一二三四'[number++ % 4]+'、'+basketSections[group],26))}
    blocks.push(...await wordBlocks(editionFace(item.q),(i+1)+'．'));
    if(teacher){
      blocks.push(...await wordBlocks(item.answer,'【答案】'),...await wordBlocks(item.analysis,'【解析】'));
      if(item.source)blocks.push(...await wordBlocks(plainBlocks(item.source),'【来源】'));
    }
  }
  if(template){
    return docx.patchDocument({outputType:'blob',data:await template.file.arrayBuffer(),recursive:false,keepOriginalStyles:true,patches:{
      '试卷名称':{type:docx.PatchType.PARAGRAPH,children:[new docx.TextRun(edition.name)]},
      '试题':{type:docx.PatchType.DOCUMENT,children:blocks}
    }});
  }
  return docx.Packer.toBlob(new docx.Document({styles:{default:{document:{run:{font:'宋体',size:22}}}},sections:[{properties:{page:{size:{width:11906,height:16838},margin:{top:1080,bottom:1080,left:1260,right:1260}}},children:blocks}]}));
}
