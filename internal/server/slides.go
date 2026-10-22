package server

// slidesDeck renders a `slides` artifact: Markdown split into slides by lines holding only
// `---`, laid out on a 1280×720 stage scaled to the frame. Arrow keys, Space, PageUp/Down,
// Home/End, clicks on either half and swipes move between slides; F toggles fullscreen,
// N the speaker notes (text after a line starting `Note:`). The slide number lives in the
// URL hash. Printing gives one slide per page. A slide holding only an h1 and up to two
// short paragraphs becomes a centered title slide.
const slidesDeck = `<style>
:root{--canvas:#faf9f5;--ink:#141413;--body:#3d3d3a;--muted:#6c6a64;--primary:#cc785c;--hairline:#e6dfd8;--card:#efe9de;--display:Copernicus,"Tiempos Headline","Cormorant Garamond","EB Garamond",Garamond,"Times New Roman",serif;--sans:StyreneB,Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}
html,body{height:100%;margin:0;background:#1c1b19;overflow:hidden}
#deck{position:absolute;left:50%;top:50%;width:1280px;height:720px;transform-origin:center}
.slide{position:absolute;inset:0;display:none;box-sizing:border-box;padding:72px 96px;background:var(--canvas);color:var(--body);font:28px/1.45 var(--sans);overflow:hidden}
.slide.active,.measuring .slide{display:flex;flex-direction:column}.measuring .slide:not(.active){visibility:hidden}
.slide h1,.slide h2,.slide h3{font-family:var(--display);font-weight:400;color:var(--ink);letter-spacing:-.02em;margin:0 0 28px}
.slide h1{font-size:76px;line-height:1.05}.slide h2{font-size:54px;line-height:1.1}.slide h3{font-size:38px}
.slide p,.slide ul,.slide ol{margin:0 0 20px}.slide li{margin:6px 0}.slide li::marker{color:var(--primary)}
.slide a{color:var(--primary)}.slide strong{color:var(--ink)}
.slide img,.slide svg,.slide video{max-width:100%;max-height:440px;object-fit:contain}
.slide pre{background:#181715;color:#faf9f5;border-radius:12px;padding:24px 28px;font-size:20px;line-height:1.5;overflow:auto;white-space:pre}
.slide pre code.hljs{color:inherit}.slide pre.mermaid{background:none;color:inherit;text-align:center}
.slide code{font-family:"JetBrains Mono",ui-monospace,SFMono-Regular,Consolas,monospace;font-size:.85em}
.slide table{border-collapse:collapse;font-size:22px}.slide td,.slide th{border-bottom:1px solid var(--hairline);padding:10px 16px;text-align:left}
.slide blockquote{margin:0;padding-left:28px;border-left:4px solid var(--primary);font-family:var(--display);font-size:40px;color:var(--ink)}
.slide.title{justify-content:center;text-align:center}.slide.title h1{font-size:96px}.slide.title p{color:var(--muted);font-size:32px}
.notes{display:none}
#bar{position:fixed;left:0;right:0;bottom:0;height:4px;background:#ffffff1f}#progress{height:100%;background:#cc785c;transition:width .2s}
#count{position:fixed;right:14px;bottom:12px;font:12px/1 var(--sans);color:#a8a49c;letter-spacing:.06em}
#speaker{position:fixed;left:12px;right:12px;bottom:16px;max-height:30%;overflow:auto;background:#141312ee;color:#f5f2ec;font:16px/1.5 var(--sans);padding:14px 18px;border-radius:10px;display:none}
body.show-notes #speaker{display:block}
@media print{@page{size:1280px 720px;margin:0}html,body{height:auto;overflow:visible;background:none}#deck{position:static;transform:none!important}.slide{position:relative;display:flex!important;flex-direction:column;break-after:page;width:1280px;height:720px}#bar,#count,#speaker{display:none!important}}
</style><div id="deck"></div><div id="bar"><div id="progress"></div></div><div id="count"></div><div id="speaker"></div>
<script>(()=>{
const deck=document.getElementById('deck'),parts=source.replace(/\r\n/g,'\n').split(/\n---[ \t]*\n/);
const slides=parts.map(part=>{const note=part.split(/\n(?:Notes?|Speaker notes?):[ \t]*/i);const s=document.createElement('section');s.className='slide';s.innerHTML=marked.parse(note[0]);const n=document.createElement('div');n.className='notes';n.textContent=note.slice(1).join('\n').trim();s.append(n);
const kids=[...s.children].filter(e=>e!==n);if(kids[0]&&kids[0].tagName==='H1'&&kids.length<=3&&kids.slice(1).every(e=>e.tagName==='P'&&e.textContent.length<160))s.classList.add('title');deck.append(s);return s});
render(deck);
let i=0;
const fit=()=>{const k=Math.min(innerWidth/1280,innerHeight/720);deck.style.transform='translate(-50%,-50%) scale('+k+')'};
const show=n=>{i=Math.max(0,Math.min(slides.length-1,n));slides.forEach((s,j)=>s.classList.toggle('active',j===i));document.getElementById('count').textContent=(i+1)+' / '+slides.length;document.getElementById('progress').style.width=((i+1)/slides.length*100)+'%';document.getElementById('speaker').textContent=slides[i].querySelector('.notes').textContent||'No notes for this slide.';if(location.hash!=='#'+(i+1))history.replaceState(null,'','#'+(i+1))};
addEventListener('resize',fit);fit();show((parseInt(location.hash.slice(1))||1)-1);
const key=k=>{if(['ArrowRight','ArrowDown','PageDown',' ','Enter'].includes(k))show(i+1);else if(['ArrowLeft','ArrowUp','PageUp','Backspace'].includes(k))show(i-1);else if(k==='Home')show(0);else if(k==='End')show(slides.length-1);
else if(k==='f'||k==='F'){document.fullscreenElement?document.exitFullscreen():document.documentElement.requestFullscreen&&document.documentElement.requestFullscreen().catch(()=>{})}else if(k==='n'||k==='N')document.body.classList.toggle('show-notes');else return false;return true};
addEventListener('keydown',e=>{if(!(e.target.closest&&e.target.closest('input,textarea,select,[contenteditable]'))&&key(e.key))e.preventDefault()});
// The page embedding the deck forwards navigation keys pressed while it has focus.
addEventListener('message',e=>{if(e.source===parent&&e.data&&e.data.type==='shartifacts-key')key(e.data.key)});
addEventListener('click',e=>{if(e.defaultPrevented||e.target.closest('a,button,input,select,textarea,video,audio,details,#speaker')||getSelection().toString())return;show(e.clientX<innerWidth/3?i-1:i+1)});
let x0=null;addEventListener('touchstart',e=>x0=e.touches[0].clientX,{passive:true});addEventListener('touchend',e=>{if(x0===null)return;const dx=e.changedTouches[0].clientX-x0;x0=null;if(Math.abs(dx)>40)show(dx<0?i+1:i-1)});
})();</script>`
