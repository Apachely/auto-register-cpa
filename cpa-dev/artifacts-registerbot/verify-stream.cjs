const fs = require('node:fs');
const path = require('node:path');
const root = path.resolve(__dirname, '../..');
const key = fs.readFileSync(path.join(root, 'admin-key.txt'), 'utf8').trim();
const base = 'http://127.0.0.1:8317/v8/management/registerbot';
const headers = {Authorization:`Bearer ${key}`};
async function replay(after) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 15000);
  try {
    const response = await fetch(`${base}/events?after=${after}`, {headers,signal:controller.signal});
    if(response.status!==200) throw new Error(`SSE status ${response.status}`);
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let source = '';
    while (!source.includes('event: ready\n')) {
      const {value,done} = await reader.read();
      if(done) throw new Error('SSE ended before replay ready');
      source += decoder.decode(value,{stream:true});
    }
    await reader.cancel();
    return [...source.matchAll(/^id: (\d+)$/gm)].map((match)=>Number(match[1]));
  } finally {clearTimeout(timer);controller.abort();}
}
(async()=>{
  const unauthenticated=await fetch(base);
  const first=await replay(0);
  const after=first[Math.floor(first.length/2)]??0;
  const resumed=await replay(after);
  const ascending = (items)=>items.every((value,index)=>index===0||value>items[index-1]);
  const result={unauthenticatedStatus:unauthenticated.status,initialEvents:first.length,after,replayedEvents:resumed.length,ordered:ascending(first)&&ascending(resumed),onlyAfterCursor:resumed.every((value)=>value>after),retainedTailPresent:first.filter((value)=>value>after).every((value)=>resumed.includes(value))};
  fs.writeFileSync(path.join(__dirname,'verification/sse-live.json'),JSON.stringify(result,null,2));
  console.log(JSON.stringify(result));
  if(!result.ordered||!result.onlyAfterCursor||!result.retainedTailPresent||result.unauthenticatedStatus!==401) process.exitCode=1;
})().catch((error)=>{console.error(error.message);process.exitCode=1;});
