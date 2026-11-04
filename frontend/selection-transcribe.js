// A photo can contain multiple questions and identifying headers. Send only the
// teacher-selected region. Applying a transcription is separate from saving it.
const PhotoTranscribe={
  props:['src','connection'],
  emits:['apply'],
  data(){return {anchor:null,area:null,busy:false,request:0,text:'',picture:'',notice:'',includeFigure:false}},
  watch:{src(){this.reset()}},
  beforeUnmount(){this.request++},
  methods:{
    reset(){this.request++;this.busy=false;this.anchor=this.area=null;this.text=this.picture=this.notice='';this.includeFigure=false},
    point(event){
      const rect=this.$refs.image.getBoundingClientRect();
      const clamp=n=>Math.min(1,Math.max(0,n));
      return {x:clamp((event.clientX-rect.left)/rect.width),y:clamp((event.clientY-rect.top)/rect.height)};
    },
    begin(event){this.reset();this.anchor=this.point(event);event.currentTarget.setPointerCapture(event.pointerId)},
    move(event){
      if(!this.anchor)return;
      const a=this.anchor,b=this.point(event);
      this.area={x:Math.min(a.x,b.x),y:Math.min(a.y,b.y),w:Math.abs(a.x-b.x),h:Math.abs(a.y-b.y)};
    },
    async recognize(manual=false){
      const token=++this.request;this.notice='';this.text='';this.picture='';this.busy=true;
      try{
        const img=this.$refs.image;await img.decode();
        if(token!==this.request)return;
        const a=this.area||{x:0,y:0,w:1,h:1};
        const canvas=document.createElement('canvas');
        canvas.width=Math.round(a.w*img.naturalWidth);canvas.height=Math.round(a.h*img.naturalHeight);
        if(canvas.width<8||canvas.height<8)throw Error('选区太小，请重新框选。');
        canvas.getContext('2d').drawImage(img,a.x*img.naturalWidth,a.y*img.naturalHeight,canvas.width,canvas.height,0,0,canvas.width,canvas.height);
        const picture=canvas.toDataURL('image/jpeg',0.95);
        if(manual){this.picture=picture;this.notice='请在下方填写这道题，再点「填入题面」。';return}
        const prompt='仅转写选区中的一道印刷生物题。保留题号、选项、小问、空格、单位及上下标。忽略页眉、班级、姓名、手写答案和批改痕迹，不解题，不猜缺字，无法辨认处写〔待核〕。若含多道题或没有可读题干，仅返回 MULTIPLE 或 UNREADABLE。其余情况仅输出题面纯文本。';
        const text=await requestQuestionAI(this.connection,prompt,[picture]);
        if(token!==this.request)return;
        if(text==='MULTIPLE')throw Error('选区包含多道题，请框出其中一道。');
        if(text==='UNREADABLE')throw Error('未读到清晰题面，请调整选区或重新拍摄。');
        this.text=text;this.picture=picture;
      }catch(e){if(token===this.request)this.notice=e.message}
      finally{if(token===this.request)this.busy=false}
    }
  },
  template:`<section class="ocr-tool"><h3>照片转文字</h3><p>整页照片请拖动框出一道题，避开姓名和其他题。单题照片可直接识别整张。</p>
  <div class="ocr-frame" @pointerdown.prevent="begin" @pointermove="move" @pointerup="move($event);anchor=null" @pointercancel="anchor=null"><img ref="image" :src="src" draggable="false" alt="待识别的照片"><span v-if="area" :style="{left:area.x*100+'%',top:area.y*100+'%',width:area.w*100+'%',height:area.h*100+'%'}"></span></div>
  <div class="row"><button type="button" :disabled="busy||!connection" @click="recognize(false)">{{busy?'正在识别…':area?'识别选区文字':'识别整张照片'}}</button><button v-if="area" type="button" :disabled="busy" @click="recognize(true)">采用选区手动录题</button><button v-if="area" type="button" @click="reset">清除选区</button></div>
  <small>{{connection?'点击识别会把所选图像发送给已配置的 AI 模型。':'请先在设置中连接支持图片的 AI 模型，或手动填写题面。'}}</small>
  <p role="status">{{notice}}</p><template v-if="picture"><label>核对转写结果<textarea v-model="text" rows="6"></textarea></label>
  <label><input type="checkbox" v-model="includeFigure"> 将选区也作为题图（导出时可见）</label>
  <button type="button" :disabled="!text.trim()" @click="$emit('apply',{text:text.trim(),picture,includeFigure});notice='已填入题面，核对后保存。'">填入题面</button></template></section>`
};
