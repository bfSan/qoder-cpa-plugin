const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname,'panel.html'),'utf8');
const source = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1].replace(/\nloadInitial\(\);\s*$/,'');
const STORE='qoder-oauth-pending-v1';
function element(){
 const set=new Set(); return {hidden:false,value:'',innerHTML:'',textContent:'',dataset:{},style:{},disabled:false,
 classList:{add:x=>set.add(x),remove:x=>set.delete(x),contains:x=>set.has(x)},addEventListener(){},focus(){},
 querySelectorAll(){return []},querySelector(){return element()},appendChild(){},remove(){}};
}
function harness(storage=new Map(),customStorage){
 const elems=new Map(),timers=new Map(),toasts=[],opened=[];let clock=1900000000000,id=0;
 class Clock extends Date{static now(){return clock}}
 const document={getElementById(x){if(!elems.has(x))elems.set(x,element());return elems.get(x)},createElement:element,querySelectorAll(){return []},addEventListener(){}};
 const context={document,Date:Clock,sessionStorage:customStorage||{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,String(v)),removeItem:k=>storage.delete(k)},localStorage:{getItem(){return null}},location:{href:'http://localhost/panel',search:'',hash:'',pathname:'/panel',host:'localhost'},history:{replaceState(){}},navigator:{},URL,URLSearchParams,TextEncoder,TextDecoder,Uint8Array,console,
 setTimeout(fn,ms){timers.set(++id,{fn,ms});return id},clearTimeout(i){timers.delete(i)},setInterval(){},clearInterval(){},fetch(){throw Error('real network prohibited')},requestAnimationFrame(f){f()},open(){const w={location:{},closed:false,close(){this.closed=true}};opened.push(w);return w}};
 context.window=context;context.self=context;context.top=context;vm.createContext(context);vm.runInContext(source,context);
 context.toast=(...args)=>toasts.push(args);context.load=async()=>true;
 return {c:context,document,storage,elems,timers,toasts,opened,run:s=>vm.runInContext(s,context),now:()=>clock,advance:n=>clock+=n};
}
function response(h,state,region='cn'){return {state,region,url:'https://qoder.com/authorize?state='+state,expires_at:new Date(h.now()+600000).toISOString(),persistent:true,save_owner:'panel'}}

test('OAuth stores only bounded session identifiers and restores several pending flows',async()=>{
 const h=harness();let n=0;h.c.api=async()=>response(h,'state-'+(++n));
 await h.c.startOAuthLogin(element(),'cn');await h.c.startOAuthLogin(element(),'intl');
 const stored=JSON.parse(h.storage.get(STORE));assert.equal(stored.length,2);
 for(const r of stored)assert.deepEqual(Object.keys(r).sort(),['deadline','region','state']);
 assert.doesNotMatch(h.storage.get(STORE),/verifier|token|https|url/);
 const restored=harness(h.storage);restored.c.restorePendingLogins();
 assert.equal(restored.document.getElementById('oauthResumeBtn').hidden,false);
 assert.equal(restored.run('pendingLogins.length'),2);
 restored.c.api=async()=>({status:'wait'});await restored.c.resumeOAuthLogin();
 assert.equal(restored.timers.size,1);
});

test('OAuth reads RFC3339 deadlines and does not guess a fresh window after expiry',()=>{
 const h=harness();h.storage.set(STORE,JSON.stringify([{state:'expired',deadline:h.now()-1,region:'cn'},{state:'too-long',deadline:h.now()+700000,region:'intl'},{state:'valid',deadline:h.now()+5000,region:'cn',token:'secret'}]));
 h.c.restorePendingLogins();const stored=JSON.parse(h.storage.get(STORE));assert.equal(stored.length,1);assert.equal(stored[0].state,'valid');assert.equal(stored[0].token,undefined);
 assert.equal(h.c.loginDeadline({expires_at:new Date(h.now()+2000).toISOString()}),h.now()+2000);
 assert.equal(h.c.loginDeadline({expires_at:(h.now()+3000)/1000}),h.now()+3000);
});

test('OAuth failed save is recoverable and cannot masquerade as successful login',async()=>{
 const h=harness();h.c.api=async()=>response(h,'state-a');await h.c.startOAuthLogin(element(),'cn');
 h.toasts.length=0;h.c.api=async()=>({status:'ok',retryable:true,error:'private-token',expires_at:new Date(h.now()+500000).toISOString()});
 await h.c.pollOAuth(h.run('oauthGeneration'));
 assert.equal(JSON.parse(h.storage.get(STORE)).length,1);assert.doesNotMatch(JSON.stringify(h.toasts),/登录成功|private-token/);
 h.c.api=async()=>({status:'ok',nickname:'account'});await h.c.pollOAuth(h.run('oauthGeneration'));
 assert.equal(JSON.parse(h.storage.get(STORE)).length,0);assert.match(JSON.stringify(h.toasts),/登录成功/);
});

test('OAuth transport errors preserve state and do not leak raw errors',async()=>{
 const h=harness();h.c.api=async()=>response(h,'state-a');await h.c.startOAuthLogin(element(),'cn');
 h.c.api=async()=>{throw Error('https://user:secret@proxy/path?verifier=private')};await h.c.pollOAuth(h.run('oauthGeneration'));
 assert.equal(JSON.parse(h.storage.get(STORE)).length,1);assert.doesNotMatch(JSON.stringify(h.toasts),/secret|verifier|private/);
});

test('OAuth late start cannot overwrite the selected session',async()=>{
 const h=harness();let finishFirst;h.c.api=()=>new Promise(r=>{finishFirst=r});const first=h.c.startOAuthLogin(element(),'cn');
 h.c.api=async()=>response(h,'second','intl');await h.c.startOAuthLogin(element(),'intl');
 finishFirst(response(h,'first','cn'));await first;
 assert.equal(h.run('oauthState'),'second');assert.equal(JSON.parse(h.storage.get(STORE)).length,2);
});

test('OAuth storage denial leaves the current login usable with honest warning',async()=>{
 const h=harness(new Map(),{getItem(){throw Error('private')},setItem(){throw Error('private')},removeItem(){}});
 h.c.restorePendingLogins();h.c.api=async()=>response(h,'state-a');await h.c.startOAuthLogin(element(),'cn');
 assert.equal(h.run('oauthState'),'state-a');assert.match(JSON.stringify(h.toasts),/请保留本页/);assert.doesNotMatch(JSON.stringify(h.toasts),/private/);
});

test('Unconfirmed checkin never uses wording or success acknowledgement as proof',async()=>{
 const h=harness(),btn=element();btn.innerHTML='签到';h.run('lastAccounts=[{auth_index:"a"}]');
 for(const r of [{success:true},{message:'今日已签到'},{success:true,reason:'already',confirmed:false},{confirmed:'true'}])assert.equal(h.c.isCheckinDoneResult(r),false);
 h.c.api=async()=>({results:[{auth_index:'a',confirmed:false,success:true,state:'pending_confirmation'}]});await h.c.checkin('a',btn);
 assert.equal(h.run('lastAccounts[0].checkin.today_checked_in'),false);assert.notEqual(btn.innerHTML,'已签到');
 assert.equal(h.toasts[0][1],'warn');assert.doesNotMatch(html,/checkinHistory|checkin-history|签到运行记录|签到记录/);
});

test('Batch checkin marks only matched confirmed accounts',async()=>{
 const h=harness(),buttons=['a','b'].map(id=>{const e=element();e.dataset.authIndex=id;return e});
 h.document.querySelectorAll=()=>buttons;h.run('lastAccounts=[{auth_index:"a"},{auth_index:"b"}]');
 h.c.api=async()=>({results:[{auth_index:'a',confirmed:true,state:'success'},{auth_index:'b',confirmed:false,state:'pending_confirmation'}],summary:{success:2}});
 await h.c.checkinAll(element());assert.equal(buttons[0].innerHTML,'已签到');assert.notEqual(buttons[1].innerHTML,'已签到');
 assert.equal(h.run('lastAccounts.filter(a=>a.checkin.today_checked_in).length'),1);
});

test('Quota state is escaped, scoped and does not imply manual reenable',()=>{
 const h=harness();const card=h.c.card({auth_index:'a',auth_id:'a',name:'test',cooling:[{scope:'account',reason:'shared_credit_exhausted',seconds:60,reset_at:'<img src=x>'}]});
 assert.match(card,/账号共享积分/);assert.match(card,/解除不改变手动禁用/);assert.match(card,/data-model=""/);assert.doesNotMatch(card,/<img/);assert.match(card,/&lt;img/);
});
