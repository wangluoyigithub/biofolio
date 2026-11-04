// Render an allow-listed SVG as PNG so saved questions and Word exports share
// the existing image path. No model-provided scripts or remote assets execute.
function safeFigureSVG(raw){
  if(typeof raw!=='string'||raw.length>400000)throw Error('重绘图数据无效或过大，请重试。');
  const doc=new DOMParser().parseFromString(raw,'image/svg+xml'),root=doc.documentElement;
  if(doc.querySelector('parsererror')||root.localName!=='svg'||root.namespaceURI!=='http://www.w3.org/2000/svg')throw Error('AI 未返回有效线条图，请重试。');
  const tags=new Set(['svg','g','defs','path','rect','circle','ellipse','line','polyline','polygon','text','tspan','marker','title','desc']);
  const attrs=new Set(['xmlns','viewBox','width','height','x','y','x1','y1','x2','y2','cx','cy','r','rx','ry','d','points','fill','stroke','stroke-width','stroke-linecap','stroke-linejoin','stroke-dasharray','fill-rule','opacity','fill-opacity','stroke-opacity','transform','font-size','font-family','font-weight','text-anchor','dominant-baseline','dx','dy','id','marker-start','marker-mid','marker-end','markerWidth','markerHeight','refX','refY','orient','markerUnits']);
  for(const el of [root,...root.querySelectorAll('*')]){
    if(!tags.has(el.localName)||el.namespaceURI!==root.namespaceURI){el.remove();continue}
    for(const a of [...el.attributes]){
      const localURL=/^url\(#[A-Za-z_][\w.-]*\)$/.test(a.value);
      if(!attrs.has(a.name)||a.value.includes('\\')||a.value.length>150000||/javascript:|data:|https?:|@import|expression\(/i.test(a.value)||(/url\s*\(/i.test(a.value)&&!localURL))el.removeAttribute(a.name);
    }
  }
  const box=(root.getAttribute('viewBox')||'').trim().split(/[\s,]+/).map(Number);
  if(box.length!==4||!box.every(Number.isFinite)||box[2]<=0||box[3]<=0||box[2]>10000||box[3]>10000)throw Error('重绘图尺寸无效，请重试。');
  if(!root.querySelector('path,line,circle,ellipse,rect,polygon,polyline'))throw Error('没有得到可用的图形，请重试。');
  root.setAttribute('width',String(box[2]));root.setAttribute('height',String(box[3]));
  return new XMLSerializer().serializeToString(root);
}
async function figureCanvas(src,area){
  const img=new Image();img.src=src;await img.decode();
  const a=area||{x:0,y:0,w:1,h:1};
  const w=Math.round(a.w*img.naturalWidth),h=Math.round(a.h*img.naturalHeight);
  if(w<8||h<8)throw Error('选区太小，请重新框选。');
  const scale=Math.min(1,2000/Math.max(w,h)),canvas=document.createElement('canvas');
  canvas.width=Math.max(1,Math.round(w*scale));canvas.height=Math.max(1,Math.round(h*scale));
  const ctx=canvas.getContext('2d');ctx.fillStyle='white';ctx.fillRect(0,0,canvas.width,canvas.height);
  ctx.drawImage(img,a.x*img.naturalWidth,a.y*img.naturalHeight,w,h,0,0,canvas.width,canvas.height);
  return canvas;
}
function enhanceFigure(canvas,strength){
  const ctx=canvas.getContext('2d'),data=ctx.getImageData(0,0,canvas.width,canvas.height),pixels=data.data;
  const w=canvas.width,h=canvas.height,stride=w+1,integral=new Float64Array((w+1)*(h+1));
  for(let y=0;y<h;y++){
    let sum=0;
    for(let x=0;x<w;x++){
      const i=(y*w+x)*4,gray=.299*pixels[i]+.587*pixels[i+1]+.114*pixels[i+2];
      sum+=gray;integral[(y+1)*stride+x+1]=integral[y*stride+x+1]+sum;
    }
  }
  const radius=Math.max(8,Math.round(Math.min(w,h)/35));
  for(let y=0;y<h;y++)for(let x=0;x<w;x++){
    const left=Math.max(0,x-radius),right=Math.min(w,x+radius+1),top=Math.max(0,y-radius),bottom=Math.min(h,y+radius+1);
    const mean=(integral[bottom*stride+right]-integral[top*stride+right]-integral[bottom*stride+left]+integral[top*stride+left])/((right-left)*(bottom-top));
    const i=(y*w+x)*4,gray=.299*pixels[i]+.587*pixels[i+1]+.114*pixels[i+2],value=Math.max(0,Math.min(255,255-(mean-gray)*strength));
    pixels[i]=pixels[i+1]=pixels[i+2]=value;pixels[i+3]=255;
  }
  ctx.putImageData(data,0,0);return canvas.toDataURL('image/png');
}
const FigureTool={
  props:['src','connection'],emits:['apply','configure'],
  data(){return {area:null,anchor:null,preview:'',cropped:'',busy:false,run:0,strength:4,notice:'',kind:'',accepted:false}},
  watch:{src(){this.reset()}},beforeUnmount(){this.run++},
  methods:{
    reset(){this.run++;this.area=this.anchor=null;this.preview=this.cropped=this.notice=this.kind='';this.busy=false;this.accepted=false},
    point(e){const r=this.$refs.image.getBoundingClientRect(),clamp=n=>Math.max(0,Math.min(1,n));return {x:clamp((e.clientX-r.left)/r.width),y:clamp((e.clientY-r.top)/r.height)}},
    begin(e){if(this.busy)return;this.anchor=this.point(e);e.currentTarget.setPointerCapture(e.pointerId);this.preview=this.cropped='';this.accepted=false},
    move(e){if(!this.anchor)return;const a=this.anchor,b=this.point(e);this.area={x:Math.min(a.x,b.x),y:Math.min(a.y,b.y),w:Math.abs(a.x-b.x),h:Math.abs(a.y-b.y)}},
    async process(kind){
      const run=++this.run;this.busy=true;this.notice='';this.preview='';this.accepted=false;
      try{
        const canvas=await figureCanvas(this.src,this.area);if(run!==this.run)return;
        const cropped=canvas.toDataURL('image/png');this.cropped=cropped;let result=cropped;
        if(kind==='enhance')result=enhanceFigure(canvas,this.strength);
        if(kind==='redraw'){
          const prompt='将选区内的高中生物示意图忠实重绘为白底黑色线条图，保留所有细胞、结构、相对位置、连线、箭头方向、虚实线、图号和文字标注。只重绘图，不抄题干、选项、手写答案或批改痕迹。不要解题或纠正原图，不增加或删除生物关系。看不清关键标注或箭头时不要猜，svg 留空并在 uncertainty 说明。仅返回 JSON {"svg":"完整 SVG，带 xmlns 和 viewBox，使用 path、line、circle、ellipse、text、g、defs、marker 等基本元素，禁止外部资源、image、脚本和样式表","uncertainty":"看不清的细节，没有则为空"}。';
          const response=await requestQuestionAI(this.connection,prompt,[cropped]);if(run!==this.run)return;
          const draft=JSON.parse(response.replace(/^```(?:json)?\s*|\s*```$/gi,''));
          if(draft.uncertainty)throw Error('需要核对原图：'+String(draft.uncertainty));
          const svg=safeFigureSVG(draft.svg),url=URL.createObjectURL(new Blob([svg],{type:'image/svg+xml'}));
          try{result=(await figureCanvas(url)).toDataURL('image/png')}finally{URL.revokeObjectURL(url)}
        }
        if(run!==this.run)return;this.preview=result;this.kind=kind;this.notice=kind==='redraw'?'重绘草稿已生成，请对照箭头、编号和结构标注。':'预览已生成，请核对线条、文字和有含义的深浅标记。';
      }catch(e){if(run===this.run)this.notice=e.message}
      finally{if(run===this.run)this.busy=false}
    }
  },
  template:`<section class="figure-tool"><h3>配图处理</h3><p>拖动框出配图。可先采用裁图，或去灰底增强、AI 重绘后再核对。</p>
  <div class="ocr-frame" @pointerdown.prevent="begin" @pointermove="move" @pointerup="move($event);anchor=null" @pointercancel="anchor=null"><img ref="image" :src="src" alt="待处理的原图" draggable="false"><span v-if="area" :style="{left:area.x*100+'%',top:area.y*100+'%',width:area.w*100+'%',height:area.h*100+'%'}"></span></div>
  <label>增强力度 <input type="range" min="1" max="8" step=".5" v-model.number="strength" :disabled="busy"> {{strength}}</label>
  <div class="row"><button type="button" :disabled="busy" @click="process('crop')">预览裁图</button><button type="button" :disabled="busy" @click="process('enhance')">原图增强</button><button type="button" :disabled="busy" @click="connection?process('redraw'):$emit('configure')">{{connection?'AI 重绘草稿':'配置 AI 后重绘'}}</button><button type="button" :disabled="busy" @click="reset">重新选择</button></div>
  <small>增强在本设备处理。AI 重绘会将当前选区发送给已配置的识图模型，由模型绘制线条图。</small><p role="status">{{busy?'正在处理配图…':notice}}</p>
  <template v-if="preview"><div class="figure-compare"><figure><figcaption>原图选区</figcaption><img :src="cropped" alt="原图选区"></figure><figure><figcaption>{{kind==='redraw'?'AI 重绘草稿':'处理预览'}}</figcaption><img :src="preview" alt="配图处理预览"></figure></div>
  <label v-if="kind==='redraw'"><input type="checkbox" v-model="accepted"> 已对照原图核对箭头方向、编号和结构标注</label><button type="button" class="primary" :disabled="busy||(kind==='redraw'&&!accepted)" @click="$emit('apply',preview)">采用此配图</button></template></section>`
};
