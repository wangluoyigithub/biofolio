// AI regions and text remain editable until the teacher applies one question.
function printedQuestionSource(text){
  const head=text.trim().replace(/^(?:例\s*\d+\s*[.．、]?|第?\s*\d+\s*题?\s*[.．、])\s*/,'').replace(/^[（(](?:多选|单选|多选题|单选题)[）)]\s*/,'');
  const embedded=head.match(/^\s*(?:第?\s*\d+\s*题?\s*[.．、]?\s*)?(?:[（(]\d+分[)）]\s*)?[（(]((?:19|20)\d{2}\s*[·•・.．]\s*[^（）()\n]+)[）)]/);
  return embedded?.[1]?.trim() || head.match(/^(?:来源|题目来源)\s*[：:]\s*([^\n]+)/)?.[1]?.trim() || '';
}
function photoSource(text,source){
  const candidate=typeof source==='string'?source.trim().replace(/^[（(]|[）)]$/g,''):'';
  return candidate && text.replace(/\s/g,'').includes(candidate.replace(/\s/g,''))?candidate:printedQuestionSource(text);
}
function photoQuestions(reply){
  const parsed=JSON.parse(reply.replace(/^```(?:json)?\s*|\s*```$/gi,''));
  if(!Array.isArray(parsed.questions)||!parsed.questions.length||parsed.questions.length>60)throw Error('未识别到题目，请调整照片或手动框选。');
  return parsed.questions.map((q,i)=>{
    const b=q.box;
    if(!Array.isArray(b)||b.length!==4||!b.every(Number.isFinite)||b[0]<0||b[1]<0||b[2]<=0||b[3]<=0||b[0]+b[2]>1.001||b[1]+b[3]>1.001||typeof q.text!=='string'||!q.text.trim())throw Error('AI 返回的题框或文字不完整，请重新识别或手动框选。');
    return {label:String(q.number||i+1),area:{x:b[0],y:b[1],w:Math.min(b[2],1-b[0]),h:Math.min(b[3],1-b[1])},text:q.text.trim(),source:photoSource(q.text,q.source),figure:typeof q.figure==='string' && /^data:image\/(?:png|jpeg);base64,[a-z0-9+/=]+$/i.test(q.figure)?q.figure:'',includeFigure:q.hasFigure===true};
  });
}
const PhotoTranscribe={
  components:{FigureTool},
  props:['src','connection','draftId'],emits:['apply','configure'],
  data(){return {anchor:null,manualArea:null,busy:false,request:0,manualText:'',manualSource:'',picture:'',notice:'',manualFigure:false,manualFigureImage:'',figureInput:'',questions:[],selected:-1,draftVersion:0}},
  computed:{
    current(){return this.questions[this.selected]},
    area:{get(){return this.current?.area||this.manualArea},set(v){if(this.current)this.current.area=v;else this.manualArea=v}},
    text:{get(){return this.current?.text??this.manualText},set(v){if(this.current)this.current.text=v;else this.manualText=v}},
    figure:{get(){return this.current?.figure||this.manualFigureImage},set(v){if(this.current)this.current.figure=v;else this.manualFigureImage=v}},
    source:{get(){return this.current?.source??this.manualSource},set(v){if(this.current)this.current.source=v;else this.manualSource=v}},
    includeFigure:{get(){return this.current?.includeFigure??this.manualFigure},set(v){if(this.current)this.current.includeFigure=v;else this.manualFigure=v}}
  },
  mounted(){this.loadDraft()},watch:{src(){this.reset();this.loadDraft()}},beforeUnmount(){this.request++},
  methods:{
    async loadDraft(){
      if(!this.draftId)return;
      const token=++this.request;this.busy=true;
      try{
        const draft=libraryServer?await(await readAsset('library/review-draft?paper='+encodeURIComponent(this.draftId))).json():JSON.parse(localStorage.getItem(key+':draft:'+this.draftId)||'null');
        if(token!==this.request)return;
        this.draftVersion=draft?.version||0;
        if(draft?.photoQuestions?.length){this.questions=photoQuestions(JSON.stringify({questions:draft.photoQuestions}));for(const [i,q] of this.questions.entries())if(typeof draft.photoQuestions[i].source==='string')q.source=draft.photoQuestions[i].source;this.selected=0;this.notice='已恢复上次保存的分题和转写草稿。'}
      }catch(e){if(token===this.request)this.notice='恢复草稿失败：'+e.message}
      finally{if(token===this.request)this.busy=false}
    },
    async saveDraft(){
      if(!this.draftId)return;
      const draft={paper:this.draftId,version:this.draftVersion+1,photoQuestions:this.questions.map(q=>({number:q.label,box:[q.area.x,q.area.y,q.area.w,q.area.h],text:q.text,source:q.source,figure:q.figure,hasFigure:q.includeFigure}))};
      if(libraryServer)await libraryPost('review-draft',draft);else localStorage.setItem(key+':draft:'+this.draftId,JSON.stringify(draft));
      this.draftVersion=draft.version;
    },
    async saveProgress(){
      this.busy=true;try{await this.saveDraft();this.notice='分题和文字草稿已保存，下次可继续。'}catch(e){this.notice=e.message}finally{this.busy=false}
    },
    reset(){this.request++;this.busy=false;this.anchor=this.manualArea=null;this.manualText=this.manualSource=this.manualFigureImage=this.figureInput=this.picture=this.notice='';this.manualFigure=false;this.questions=[];this.selected=-1},
    point(event){const r=this.$refs.image.getBoundingClientRect(),clamp=n=>Math.min(1,Math.max(0,n));return {x:clamp((event.clientX-r.left)/r.width),y:clamp((event.clientY-r.top)/r.height)}},
    begin(event){if(this.busy)return;this.anchor=this.point(event);event.currentTarget.setPointerCapture(event.pointerId)},
    move(event){if(!this.anchor)return;const a=this.anchor,b=this.point(event);this.area={x:Math.min(a.x,b.x),y:Math.min(a.y,b.y),w:Math.abs(a.x-b.x),h:Math.abs(a.y-b.y)};this.picture=this.figureInput='';this.figure=''},
    style(a){return {left:a.x*100+'%',top:a.y*100+'%',width:a.w*100+'%',height:a.h*100+'%'}},
    choose(i){this.selected=i;this.picture=this.figureInput='';this.notice='可直接修改文字，或在原图上重新拖出本题边框。'},
    async crop(area){
      const img=this.$refs.image;await img.decode();
      const a=area||{x:0,y:0,w:1,h:1},c=document.createElement('canvas');
      c.width=Math.round(a.w*img.naturalWidth);c.height=Math.round(a.h*img.naturalHeight);
      if(c.width<8||c.height<8)throw Error('选区太小，请重新框选。');
      c.getContext('2d').drawImage(img,a.x*img.naturalWidth,a.y*img.naturalHeight,c.width,c.height,0,0,c.width,c.height);
      return c.toDataURL('image/jpeg',0.95);
    },
    async autoRecognize(){
      const token=++this.request;this.busy=true;this.notice='AI 正在分题并转写…';
      try{
        const picture=await this.crop(null);if(token!==this.request)return;
        const prompt='识别这张高中生物试卷照片中的所有印刷题，逐题转写题干、选项和小问，保留题号附近的年份、地区、学校、考试等来源标注，并原样提取到 source 字段。来源必须能在本题 text 中找到，没印来源就留空，不根据内容猜测。忽略页眉、姓名、班级、手写答案和批改痕迹。不解题，不补写缺失内容，模糊处写〔待核〕。每题用一个紧贴完整题干、选项、配图的矩形框，框内不得含其他题或页眉。按原卷阅读顺序返回。仅返回 JSON 对象 {"questions":[{"number":"原题号","box":[x,y,width,height],"text":"完整题面文字","source":"题目上印刷的来源，没有则为空字符串","hasFigure":true}]}。坐标均相对整张图归一化到 0 到 1，以左上角为原点，box 是 x、y、宽、高。hasFigure 表示题内有必要配图或表格。不能辨认则返回空 questions。';
        const rows=photoQuestions(await requestQuestionAI(this.connection,prompt,[picture]));
        if(token!==this.request)return;
        this.questions=rows;this.selected=0;this.picture='';await this.saveDraft();if(token!==this.request)return;this.notice='已识别 '+rows.length+' 道题。请逐题核对边框、文字和配图。';
      }catch(e){if(token===this.request)this.notice=e.message}
      finally{if(token===this.request)this.busy=false}
    },
    async recognize(manual=false){
      const token=++this.request;this.notice='';this.busy=true;
      try{
        const picture=await this.crop(this.area);if(token!==this.request)return;
        if(manual){this.picture=picture;this.notice='请在下方填写这道题，再点「填入题面」。';return}
        const prompt='仅转写选区中的一道印刷生物题，保留题号、选项、小问、单位及上下标。忽略页眉、姓名、班级、手写答案和批改痕迹，不解题，不猜缺字，模糊处写〔待核〕。含多道题返回 MULTIPLE，没有可读题干返回 UNREADABLE，否则仅返回 JSON 对象 {"text":"完整题面文字，保留原有来源标注","source":"题目上印刷的来源，没有则为空字符串"}。source 仅提取本题印刷的年份、地区、学校或考试标注，必须在 text 中原样出现，不能猜测。';
        const text=await requestQuestionAI(this.connection,prompt,[picture]);if(token!==this.request)return;
        if(text==='MULTIPLE')throw Error('选区包含多道题，请调整题框。');
        if(text==='UNREADABLE')throw Error('未读到清晰题面，请调整选区或重新拍摄。');
        let draft={text};
        if(text.startsWith('{')||text.startsWith('```'))draft=JSON.parse(text.replace(/^```(?:json)?\s*|\s*```$/gi,''));
        if(typeof draft.text!=='string'||!draft.text.trim())throw Error('未返回有效题面，请重新识别。');
        this.text=draft.text.trim();this.source=photoSource(this.text,draft.source);this.picture=picture;
      }catch(e){if(token===this.request)this.notice=e.message}
      finally{if(token===this.request)this.busy=false}
    },
    async prepareFigure(){
      const token=++this.request;this.busy=true;
      try{const src=await this.crop(this.area);if(token===this.request)this.figureInput=src}catch(e){if(token===this.request)this.notice=e.message}finally{if(token===this.request)this.busy=false}
    },
    async apply(){
      const token=++this.request;this.busy=true;
      try{const picture=await this.crop(this.area);if(token!==this.request)return;if(this.questions.length)await this.saveDraft();if(token!==this.request)return;this.$emit('apply',{text:this.text.trim(),source:this.source.trim()||printedQuestionSource(this.text),picture,figure:this.figure,includeFigure:this.includeFigure});this.notice='本题已填入下方表单，核对答案和解析后保存。'}
      catch(e){if(token===this.request)this.notice=e.message}
      finally{if(token===this.request)this.busy=false}
    }
  },
  template:`<section class="ocr-tool"><h3>照片转文字</h3>
  <div class="row"><button type="button" class="primary" :disabled="busy" @click="connection?autoRecognize():$emit('configure')">{{busy?'正在处理…':connection?'AI 自动分题并转写':'配置 AI，自动分题并转写'}}</button><button v-if="connection" type="button" :disabled="busy" @click="$emit('configure')">AI 设置</button></div>
  <small>自动分题会将整张照片发送给已配置的 AI。识别后可调整题框和文字，逐题核对收录。</small>
  <div v-if="questions.length" class="row ocr-questions"><button v-for="(q,i) in questions" type="button" :disabled="busy" :aria-pressed="i===selected" :class="{on:i===selected}" @click="choose(i)">第 {{q.label}} 题</button></div>
  <p>{{questions.length?'拖动可重新框选当前题，识别选区可重新转写。':'也可手动拖出一道题，再识别选区。'}}</p>
  <div class="ocr-frame" @pointerdown.prevent="begin" @pointermove="move" @pointerup="move($event);anchor=null" @pointercancel="anchor=null"><img ref="image" :src="src" draggable="false" alt="待识别的照片"><span v-for="(q,i) in questions" :style="style(q.area)" :class="{'ocr-other':i!==selected}"><b>{{q.label}}</b></span><span v-if="area && !current" :style="style(area)"></span></div>
  <div class="row"><button type="button" :disabled="busy||!connection" @click="recognize(false)">{{area?'识别选区文字':'识别整张单题照片'}}</button><button v-if="area" type="button" :disabled="busy" @click="recognize(true)">采用选区手动录题</button><button type="button" :disabled="busy" @click="reset">清除识别结果</button></div>
  <button v-if="draftId && questions.length" type="button" :disabled="busy" @click="saveProgress">保存分题与转写进度</button><p role="status">{{notice}}</p><template v-if="picture||current"><label>核对转写结果<textarea v-model="text" rows="6" :disabled="busy"></textarea></label><label>识别到的来源（可修改）<input v-model="source" :disabled="busy" placeholder="题面未注明时留空"></label><button type="button" :disabled="busy" @click="prepareFigure">单独裁图 / 增强 / AI 重绘</button><figure-tool v-if="figureInput" :src="figureInput" :connection="connection" @configure="$emit('configure')" @apply="figure=$event;includeFigure=true;figureInput='';notice='配图已处理，请填入题面。'"></figure-tool><img v-if="figure" class="capture-preview" :src="figure" alt="已处理的配图"><label><input type="checkbox" v-model="includeFigure" :disabled="busy"> {{figure?'使用已处理配图':'将本题选区作为题图（有配图时请保留）'}}</label><button type="button" :disabled="busy||!text.trim()" @click="apply">填入题面</button></template></section>`
};
