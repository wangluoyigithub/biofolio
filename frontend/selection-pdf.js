// Load the offline renderer only when a teacher opens a PDF.
let pdfRuntime;
async function loadPdfRuntime(){
  if(!pdfRuntime)pdfRuntime=(async()=>{
    const embedded=document.getElementById('pdf-libraries');
    await loadTool('zip');
    const url=embedded?.dataset.url;
    const data=url?await (await readAsset(url)).arrayBuffer():embedded?embedded.textContent:await (await readAsset('../third_party/pdfjs-4.10.38.zip')).arrayBuffer();
    const zip=await JSZip.loadAsync(data,{base64:!!embedded && !url});
    async function moduleUrl(path){
      return URL.createObjectURL(new Blob([await zip.file(path).async('uint8array')],{type:'text/javascript'}));
    }
    const main=await moduleUrl('build/pdf.min.mjs');
    let pdf;
    try{pdf=await import(main)}finally{URL.revokeObjectURL(main)}
    pdf.GlobalWorkerOptions.workerSrc=await moduleUrl('build/pdf.worker.min.mjs');
    class CMaps{
      async fetch({name}){return {cMapData:await zip.file('cmaps/'+name+'.bcmap').async('uint8array'),compressionType:1}}
    }
    class Fonts{
      async fetch({filename}){return zip.file('standard_fonts/'+filename).async('uint8array')}
    }
    return {pdf,options:{useWorkerFetch:false,isEvalSupported:false,CMapReaderFactory:CMaps,StandardFontDataFactory:Fonts}};
  })().catch(err=>{pdfRuntime=null;throw err});
  return pdfRuntime;
}
const singlePdf={
  data(){return {pdfDoc:null,pdfNumber:1,pdfCrop:null,pdfAnchor:null,pdfDrawing:false,pdfCanvas:null}},
  methods:{
    clearPdf(){
      this.pdfDoc?.destroy();this.pdfDoc=null;this.pdfNumber=1;this.pdfCanvas=null;this.pdfCrop=this.pdfAnchor=null;this.pdfDrawing=false;
    },
    async openSinglePdf(file,seq){
      const {pdf,options}=await loadPdfRuntime();
      const doc=await pdf.getDocument({...options,data:await file.arrayBuffer()}).promise;
      if(seq!==this.fileSequence){await doc.destroy();return}
      this.pdfDoc=Vue.markRaw(doc);
      await this.drawPdfPage(1);
    },
    async drawPdfPage(number){
      const seq=++this.fileSequence;
      this.pdfNumber=number;this.readingSingle=true;this.photo=null;this.singleMatch=null;this.singleError='';
      this.pdfAnchor=this.pdfCrop=null;this.pdfCanvas=null;
      try{
        const page=await this.pdfDoc.getPage(number);
        const original=page.getViewport({scale:1});
        const view=page.getViewport({scale:Math.min(2.5,2400/Math.max(original.width,original.height))});
        const canvas=document.createElement('canvas');canvas.width=Math.ceil(view.width);canvas.height=Math.ceil(view.height);
        await page.render({canvasContext:canvas.getContext('2d'),viewport:view}).promise;
        if(seq!==this.fileSequence)return;
        this.pdfCanvas=Vue.markRaw(canvas);
        await this.$nextTick();
        const target=this.$refs.pdfPage;if(!target)return;
        target.width=canvas.width;target.height=canvas.height;target.getContext('2d').drawImage(canvas,0,0);
      }catch(e){if(seq===this.fileSequence)this.singleError='PDF 页面读取失败：'+e.message}
      finally{if(seq===this.fileSequence)this.readingSingle=false}
    },
    cropPoint(event){
      const b=this.$refs.pdfPage.getBoundingClientRect();
      const bound=n=>Math.max(0,Math.min(1,n));
      return {x:bound((event.clientX-b.left)/b.width),y:bound((event.clientY-b.top)/b.height)};
    },
    startCrop(event){
      this.photo=null;this.singleMatch=null;this.pdfCrop=null;this.pdfDrawing=true;
      this.pdfAnchor=this.cropPoint(event);event.currentTarget.setPointerCapture(event.pointerId);
    },
    moveCrop(event){
      if(!this.pdfDrawing)return;
      const a=this.pdfAnchor,b=this.cropPoint(event);
      this.pdfCrop={x:Math.min(a.x,b.x),y:Math.min(a.y,b.y),w:Math.abs(a.x-b.x),h:Math.abs(a.y-b.y)};
    },
    async adoptCrop(){
      const seq=this.fileSequence,canvas=this.pdfCanvas;if(!canvas)return;
      const crop=this.pdfCrop||{x:0,y:0,w:1,h:1};
      const out=document.createElement('canvas');out.width=Math.round(canvas.width*crop.w);out.height=Math.round(canvas.height*crop.h);
      if(out.width<4||out.height<4){this.singleError='选区太小，请重新框选。';return}
      out.getContext('2d').drawImage(canvas,canvas.width*crop.x,canvas.height*crop.y,out.width,out.height,0,0,out.width,out.height);
      const blob=await new Promise(resolve=>out.toBlob(resolve,'image/png'));
      if(seq!==this.fileSequence||!blob)return;
      this.photo=new File([blob],'PDF题图.png',{type:'image/png'});this.singleError='';
    }
  }
};
