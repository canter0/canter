import test from 'node:test';
import assert from 'node:assert/strict';
import { artifactDraftChanged, suggestArtifactMerge } from '../src/lib/artifact-merge.ts';
const text = value => ({kind:'text',text:value});
const csv = rows => ({kind:'csv',rows});
const mergeText = (base,local,current) => suggestArtifactMerge(text(base),{text:local,rows:[]},text(current));
const mergeCSV = (base,local,current) => suggestArtifactMerge(csv(base),{text:'',rows:local},csv(current));

test('independent text replacements preserve both contributions and exact line endings',()=>{
 const base='# Review\r\nOwner: pending\r\nPeer: pending\r\n';
 const ours=base.replace('Owner: pending','Owner: ready'),theirs=base.replace('Peer: pending','Peer: reviewed');
 const result=mergeText(base,ours,theirs);
 assert.equal(result.conflicted,false);
 assert.equal(result.draft.text,'# Review\r\nOwner: ready\r\nPeer: reviewed\r\n');
 assert.deepEqual(mergeText(base,theirs,ours),result);
});
test('overlapping replacements and same-location inserts require review without changing the draft',()=>{
 for(const [base,ours,theirs] of [['label\n','ours\n','theirs\n'],['a\nb\n','a\nx\nb\n','a\ny\nb\n'],['','ours','theirs'],['a\nb\n','a\n','a\nB\n']]){
  const result=mergeText(base,ours,theirs);assert.equal(result.conflicted,true);assert.equal(result.draft.text,ours);
 }
});
test('one unchanged side, identical changes, empty results and Unicode are preserved',()=>{
 for(const [base,ours,theirs,expected] of [['old','old','new','new'],['old','new','old','new'],['old','new','new','new'],['old','','old',''],['猫\n犬','犬\n犬','猫\n鳥','犬\n鳥']]){
  const result=mergeText(base,ours,theirs);assert.equal(result.conflicted,false);assert.equal(result.draft.text,expected);
 }
});
test('independent insert and delete use base positions; ambiguous edit boundaries stay manual',()=>{
 assert.equal(mergeText('a\nb\nc\nd\n','a\nx\nb\nc\nd\n','a\nb\nc\n').draft.text,'a\nx\nb\nc\n');
 assert.equal(mergeText('a\nb\n','a\nx\nb\n','A\nb\n').conflicted,true);
});
test('multiple edit regions conservatively fall back rather than guessing a partial merge',()=>{
 const result=mergeText('a\nb\nc\n','A\nb\nC\n','a\nB\nc\n');assert.equal(result.conflicted,true);
});
test('different table cells combine without evaluating formulas or mutating source arrays',()=>{
 const base=[['Owner','pending'],['Peer','pending']],ours=[['Owner','=1+1'],['Peer','pending']],theirs=[['Owner','pending'],['Peer','<script>text</script>']];
 const snapshot=JSON.stringify([base,ours,theirs]),result=mergeCSV(base,ours,theirs);
 assert.equal(result.conflicted,false);assert.deepEqual(result.draft.rows,[['Owner','=1+1'],['Peer','<script>text</script>']]);assert.equal(JSON.stringify([base,ours,theirs]),snapshot);
});
test('same-cell disagreement and structural edits require review; identical or single-sided structural edits do not',()=>{
 const base=[['name','pending']],ours=[['name','ours']],theirs=[['name','theirs']];
 assert.equal(mergeCSV(base,ours,theirs).conflicted,true);
 assert.equal(mergeCSV(base,ours,[...base,['extra','row']]).conflicted,true);
 assert.equal(mergeCSV(base,base,[...base,['extra','row']]).conflicted,false);
 assert.equal(mergeCSV(base,[],[]).conflicted,false);
 assert.equal(mergeCSV([['a'],['b','c']],[['A'],['b','c']],[['a'],['b','C']]).conflicted,false);
});
test('a file-format change cannot produce a merge proposal',()=>{
 const draft={text:'my draft',rows:[]};const result=suggestArtifactMerge(text('before'),draft,csv([['other']]));
 assert.equal(result.conflicted,true);assert.equal(result.draft,draft);
});
test('row moves and edits in the same row never reassign a value to the wrong record',()=>{
 const base=[['Owner','pending'],['Peer','pending']];
 const moved=[['Peer','pending'],['Owner','pending']],edited=[['Owner','ready'],['Peer','pending']];
 assert.equal(mergeCSV(base,moved,edited).conflicted,true);
 assert.equal(mergeCSV(base,[['Renamed owner','pending'],['Peer','pending']],edited).conflicted,true);
});

test('unchanged drafts do not create empty revisions, including tables copied into new arrays',()=>{
 assert.equal(artifactDraftChanged(text('same'),{text:'same',rows:[]}),false);
 assert.equal(artifactDraftChanged(text('same'),{text:'different',rows:[]}),true);
 assert.equal(artifactDraftChanged(csv([['a'],['b','c']]),{text:'',rows:[['a'],['b','c']]}),false);
 assert.equal(artifactDraftChanged(csv([['a']]),{text:'',rows:[['a','']]}),true);
});
