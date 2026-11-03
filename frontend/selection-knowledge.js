// 每行对应人教版目录的一节。知识点名用于显示和检索，节名用于选定授课进度。
const sectionTopics = [
  [
    ['细胞学说、生命系统的结构层次','显微镜的使用、原核细胞与真核细胞、细胞的共性'],
    ['组成细胞的元素、组成细胞的化合物、有机物的检测','自由水与结合水、无机盐的功能','糖类的元素组成、糖类的种类及分布、糖类的功能、细胞中的糖类综合、脂质的元素组成、脂质的种类及功能、细胞中的脂质综合','氨基酸的结构、肽键与脱水缩合、蛋白质的结构与功能','核苷酸的组成、DNA与RNA的比较、生物大分子与碳链骨架'],
    ['细胞膜的功能、细胞间的信息交流、流动镶嵌模型、生物膜的结构与功能、细胞壁的组成与功能','细胞器的结构与功能、线粒体与叶绿体、溶酶体与细胞自噬、细胞骨架与细胞器、分离细胞器的方法、细胞器的分离与成分、观察叶绿体、细胞质的流动、分泌蛋白的合成与运输、细胞器之间的协调配合、生物膜系统、细胞结构与功能相适应','细胞核的结构与功能、染色质与染色体、探究细胞核的功能'],
    ['渗透作用、质壁分离与复原、自由扩散与协助扩散','主动运输、胞吞与胞吐'],
    ['酶的本质与作用、酶的特性、影响酶活性的因素','ATP的结构、ATP与ADP的相互转化','有氧呼吸、无氧呼吸、细胞呼吸的应用','色素的提取与分离、光反应与暗反应、影响光合作用的因素'],
    ['细胞周期、有丝分裂、观察根尖分生区细胞的有丝分裂','细胞分化、细胞的全能性','细胞衰老、细胞凋亡与坏死']
  ],[
    ['相对性状与显隐性、分离定律、测交','自由组合定律、独立遗传的概率计算'],
    ['减数分裂、配子的形成、受精作用','基因位于染色体上的实验证据','伴性遗传、遗传系谱分析'],
    ['肺炎链球菌转化实验、噬菌体侵染实验','DNA双螺旋结构、碱基互补配对','DNA半保留复制、DNA复制的过程','基因与遗传信息'],
    ['转录、翻译、遗传密码','基因与性状、表观遗传'],
    ['基因突变、基因重组','染色体结构变异、染色体数目变异','人类遗传病的类型、遗传病的调查'],
    ['生物进化的证据','自然选择与适应','基因频率、隔离与物种形成','协同进化、生物多样性的形成']
  ],[
    ['内环境的组成、组织液与血浆和淋巴液','内环境稳态及其调节'],
    ['神经元、神经系统的组成','反射与反射弧','兴奋的传导、突触传递','神经系统的分级调节','大脑皮层的功能'],
    ['激素的发现、内分泌系统','激素的分级调节、反馈调节、血糖调节','体温调节、水盐平衡调节'],
    ['免疫系统的组成、免疫系统的功能','体液免疫、细胞免疫','过敏反应、自身免疫病、免疫缺陷病','疫苗、器官移植'],
    ['生长素的发现、生长素的作用','植物激素的种类、植物激素的相互作用','植物生长调节剂','光与植物生长发育、温度和重力对植物的影响']
  ],[
    ['种群密度的调查、种群的数量特征','种群增长模型、环境容纳量','种群数量的影响因素'],
    ['种间关系、群落的空间结构、物种丰富度','群落的主要类型','初生演替与次生演替'],
    ['生态系统的组成成分、食物链与食物网','能量流动的过程、能量传递效率','碳循环、生物富集','生态系统的信息传递','抵抗力稳定性与恢复力稳定性'],
    ['生态足迹、全球性生态环境问题','生物多样性的价值、生物多样性的保护措施','生态工程的基本原理、生态工程实例']
  ],[
    ['果酒与果醋的制作、腐乳与泡菜的制作','培养基、无菌技术、微生物的分离与计数','发酵工程的流程、发酵工程的应用'],
    ['植物组织培养、植物体细胞杂交','动物细胞培养、单克隆抗体、体细胞核移植','胚胎早期发育、体外受精与胚胎移植'],
    ['限制酶与DNA连接酶、载体','目的基因的获取、基因表达载体的构建、目的基因的导入与检测','基因工程的应用','蛋白质工程的原理'],
    ['转基因产品的安全性','生殖性克隆与治疗性克隆','生物武器与生物安全']
  ]
];
const topicName = name => String(name).replace(/^\s*(?:K\d+|\d+(?:\.\d+)+)[\s　:：·、-]+/i,'').trim();
const leaf = name => ({name,children:[],tags:[name]});
const branch = (name,children) => ({name,children,tags:[...new Set(children.flatMap(child=>child.tags))]});
const curriculumTree = biologyBooks.map(([book,chapters],b)=>branch(book,chapters.map(([chapter,...sections],c)=>
  branch(chapter,sections.map((section,s)=>section===sectionTopics[b][c][s] ? leaf(section) : branch(section,sectionTopics[b][c][s].split('、').map(leaf)))))));
function knowledgeDirectory(extra) {
  const roots=curriculumTree.map(book=>branch(book.name.replace(/^(选择性)?必修\d+\s*/,''),book.children));
  const [basic,molecules,structure,...rest]=roots[0].children;
  const [membrane,organelles,nucleus]=structure.children;
  roots[0]=branch(roots[0].name,[molecules,branch('细胞的结构和功能',[
    ...basic.children,branch('细胞膜和细胞壁',membrane.children),branch('细胞质',organelles.children),nucleus]),...rest]);
  if(extra.length) roots.push(branch('其他已收录知识点',extra.map(leaf)));
  return roots;
}
function visibleTopicRows(roots,opened,query) {
  const rows=[], term=query.trim();
  function visit(item,depth,path,matchedParent){
    const key=path+'/'+item.name;
    const matched=matchedParent || (term && item.name.includes(term));
    if(term && !matched && !item.tags.some(t=>t.includes(term)) && !containsName(item,term))return;
    const expanded=!!term || opened.includes(key);
    rows.push({item,depth,key,expanded});
    if(expanded) for(const child of item.children) visit(child,depth+1,key,matched);
  }
  function containsName(item,t){return item.children.some(c=>c.name.includes(t)||containsName(c,t))}
  for(const root of roots)visit(root,0,'',false);
  return rows;
}

const directoryTags = [...new Set(curriculumTree.flatMap(book=>book.tags))];
const directoryIndex = new Set(directoryTags);
const knowledgeBrowser = {
  data(){return {topicQuery:'',openTopics:['/分子与细胞'],selectedTopics:[]}},
  computed:{
    topicRoots(){
      const existing=this.qs.flatMap(q=>this.knowledgeOf(q));
      const extra=[...new Set(existing.filter(t=>!directoryIndex.has(t)))];
      return knowledgeDirectory(extra);
    },
    topicRows(){return visibleTopicRows(this.topicRoots,this.openTopics,this.topicQuery)}
  },
  methods:{
    selectTopic(row){
      this.node=row.item.name;
      this.source='';
      this.selectedTopics=row.item.tags;
      this.pageNo=1;
    },
    toggleTopic(row){this.openTopics=this.flip(this.openTopics,row.key)}
  }
};
