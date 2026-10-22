package server

// previewSandbox is the capability set of every preview, applied both by the CSP header
// (standalone preview URLs) and the iframe attribute (web/index.html, web/view.html).
// The origin stays opaque; network access stays governed by the CSP.
const previewSandbox = "allow-scripts allow-forms allow-modals allow-downloads allow-popups allow-popups-to-escape-sandbox allow-pointer-lock"

// previewRuntime runs before artifact code in every preview:
//   - shows uncaught errors in the page and reports them, with failed loads and CSP
//     blocks, to the embedding page, which records them for agents (get_artifact);
//   - opens links in a new tab, since the sandboxed frame cannot navigate away; links to
//     the artifact's own pages (multi-file artifacts) stay in the frame;
//   - provides window.storage, window.fs.readFile and window.claude.complete with Claude's
//     APIs (readFile reads Media library files by name). The
//     opaque-origin frame cannot reach the server, so calls go to the embedding page
//     (web/bridge.js), which saves storage for the artifact it is showing. Opened on its
//     own, storage lasts only for the page and window.claude is unavailable.
const previewRuntime = `<script>(()=>{
const show=m=>{const p=document.createElement('pre');p.id='error';p.textContent=m;(document.body||document.documentElement).append(p)};
const seen=new Set(),report=m=>{m=String(m);if(parent!==window&&seen.size<10&&!seen.has(m)){seen.add(m);parent.postMessage({type:'shartifacts-error',message:m},'*')}};
addEventListener('error',e=>{if(e.target===window){show(e.message);report(e.message+(e.lineno?' (line '+e.lineno+')':''))}else if(e.target&&e.target.tagName)report('failed to load <'+e.target.tagName.toLowerCase()+'> '+(e.target.currentSrc||e.target.src||e.target.href||''))},true);
addEventListener('unhandledrejection',e=>{show(String(e.reason));report('unhandled rejection: '+String(e.reason&&e.reason.stack||e.reason))});
addEventListener('securitypolicyviolation',e=>report('blocked by the sandbox: '+e.blockedURI+' ('+e.effectiveDirective+')'));
const dir=(location.pathname.match(/^\/(?:preview\/[^/]+\/\d+|draft-preview\/[^/]+)\//)||[])[0];
document.addEventListener('click',e=>{const a=e.target.closest&&e.target.closest('a[href]');if(!a||e.defaultPrevented)return;const h=a.getAttribute('href');if(!h||h[0]==='#'||/^javascript:/i.test(h)||a.hasAttribute('download'))return;const u=new URL(h,location.href);if(dir&&u.host===location.host&&u.pathname.startsWith(dir))return;if(!a.target||/^_(self|parent|top)$/i.test(a.target)){a.target='_blank';a.rel='noopener noreferrer'}});
const pending=new Map();let seq=0,warned=false;
addEventListener('message',e=>{const d=e.data;if(e.source!==parent||!d||d.type!=='shartifacts-result')return;const p=pending.get(d.id);if(!p)return;pending.delete(d.id);d.error?p.reject(new Error(d.error)):p.resolve(d.result)});
const memory={false:new Map(),true:new Map()};
const local=(op,a)=>{const m=memory[a.shared];if(op==='get')return m.has(a.key)?{key:a.key,value:m.get(a.key),shared:a.shared}:null;if(op==='set'){m.set(a.key,a.value);return{key:a.key,value:a.value,shared:a.shared}}if(op==='delete')return{key:a.key,deleted:m.delete(a.key),shared:a.shared};return{keys:[...m.keys()].filter(k=>k.startsWith(a.prefix)).sort(),prefix:a.prefix,shared:a.shared}};
const send=(type,a,ms)=>new Promise((resolve,reject)=>{const id=++seq;pending.set(id,{resolve,reject});parent.postMessage({type,id,...a},'*');setTimeout(()=>{if(pending.delete(id))reject(new Error(type.slice(12)+' did not respond'))},ms)});
const call=(op,a)=>{if(parent===window){if(!warned){warned=true;console.warn('window.storage lasts only for this page here; open the artifact in shartifacts to keep it.')}return Promise.resolve(local(op,a))}return send('shartifacts-storage',{op,...a},15000)};
window.fs={readFile:(path,opts)=>parent===window?Promise.reject(new Error('window.fs needs the artifact opened in shartifacts')):send('shartifacts-file',{path:String(path),encoding:opts&&opts.encoding||null},120000)};
window.claude={complete:prompt=>parent===window?Promise.reject(new Error('window.claude needs the artifact opened in shartifacts')):send('shartifacts-complete',typeof prompt==='string'?{prompt}:{messages:prompt},300000)};
window.storage={get:(key,shared=false)=>call('get',{key:String(key),shared:!!shared}),
set:(key,value,shared=false)=>typeof value==='string'?call('set',{key:String(key),value,shared:!!shared}):Promise.reject(new TypeError('window.storage values must be strings; use JSON.stringify')),
delete:(key,shared=false)=>call('delete',{key:String(key),shared:!!shared}),
list:(prefix='',shared=false)=>call('list',{prefix:String(prefix??''),shared:!!shared})};
})();</script>`
