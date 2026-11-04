// A suggestion remains an editable draft until the teacher explicitly uses it.
const IntakeSolution={
 props:['connection','face','image','answer','analysis'],emits:['apply','configure'],
 data(){return {draft:null,notice:'',busy:false,run:0}},
 watch:{face:'reset',image:'reset',answer:'reset',analysis:'reset'},
 beforeUnmount(){this.reset()},
 methods:{
  reset(){this.run++;this.draft=null;this.busy=false;this.notice=''},
  async generate(){
   this.reset();const run=this.run;this.busy=true;
   try{
    const {text,images}=aiQuestionParts({contentHtml:this.face});
    if(this.image)images.push(await imageData(this.image));
    if(!text&&!images.length)throw Error('请先填写题面或选择题图');
    const prompt='为高中生物教师起草缺少的答案和解析。题面信息不足时对应值留空，不猜测。只返回 JSON 对象，键为 answer 和 analysis，值为字符串。只填写缺项。\n题面：'+text+'\n已有答案：'+(this.answer||'')+'\n已有解析：'+(this.analysis||'');
    const reply=await requestQuestionAI(this.connection,prompt,images);
    if(run!==this.run)return;
    const parsed=JSON.parse(reply.replace(/^```(?:json)?\s*|\s*```$/gi,''));
    const take=k=>typeof parsed?.[k]==='string'?parsed[k].trim():'';
    this.draft={answer:this.answer?'':take('answer'),analysis:this.analysis?'':take('analysis')};
    if(!this.draft.answer&&!this.draft.analysis)throw Error('未得到可用草稿，请检查题面或手动补充');
   }catch(e){if(run===this.run){this.draft=null;this.notice=e.message}}
   finally{if(run===this.run)this.busy=false}
  }
 },
 template:`<div v-if="!answer||!analysis"><button type="button" @click="connection?generate():$emit('configure')" :disabled="busy">{{busy?'正在起草…':connection?'AI 起草缺少的答案和解析':'配置 AI，起草本题答案和解析'}}</button><small>将题面和相关题图发送给已配置的 AI。</small><p role="status">{{notice}}</p><template v-if="draft"><p v-if="draft.answer" class="preserve">答案草稿　{{draft.answer}}</p><p v-if="draft.analysis" class="preserve">解析草稿　{{draft.analysis}}</p><button type="button" @click="$emit('apply',draft);reset()">采用草稿并继续核对</button></template></div>`
};
