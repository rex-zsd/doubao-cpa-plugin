#!/usr/bin/env python3
"""Package allowlisted source and binaries. Never include account state."""
import argparse,hashlib,json,pathlib,tarfile
root=pathlib.Path(__file__).resolve().parents[1]
p=argparse.ArgumentParser();p.add_argument('--build-dir',required=True,type=pathlib.Path);p.add_argument('--out-dir',required=True,type=pathlib.Path);args=p.parse_args()
if not args.build_dir.is_absolute() or not args.out_dir.is_absolute():p.error('directories must be absolute')
args.out_dir.mkdir(parents=True,exist_ok=True)
version='0.1.0';prefix=f'doubao-cpa-plugin-{version}-linux-amd64'
source=[]
for name in ['cmd','internal','integration','scripts','patches','docs','examples']:
 source +=[(path,path.relative_to(root)) for path in (root/name).rglob('*') if path.is_file() and '__pycache__' not in path.parts]
source +=[(root/name,pathlib.Path(name)) for name in ['.gitignore','.gitattributes','go.mod','go.sum','README.md','README_CN.md','LICENSE','THIRD_PARTY_NOTICES.md']]

def archive(filename,entries,folder):
 path=args.out_dir/filename
 with tarfile.open(path,'w:gz',compresslevel=9) as out:
  for file,relative in sorted(entries,key=lambda pair:str(pair[1])):out.add(file,arcname=str(pathlib.Path(folder)/relative),recursive=False)
 return path
files=[]
files.append(archive(f'doubao-cpa-plugin-{version}-source.tar.gz',source,f'doubao-cpa-plugin-{version}-source'))
common=[entry for entry in source if entry[1].parts[0] in ('docs','patches') or str(entry[1]) in ['README.md','README_CN.md','LICENSE','THIRD_PARTY_NOTICES.md','scripts/install.sh']]
plugin=common+[(args.build_dir/'doubao-helper',pathlib.Path('bin/doubao-helper')),(args.build_dir/'doubao-work.so',pathlib.Path('plugins/doubao-work.so'))]
files.append(archive(f'{prefix}.tar.gz',plugin,prefix))
host=[(args.build_dir/'cli-proxy-api',pathlib.Path('bin/cli-proxy-api'))]+[entry for entry in common if str(entry[1]) in ['LICENSE','THIRD_PARTY_NOTICES.md','README_CN.md','patches/cpa-http-wire.patch']]
files.append(archive(f'cpa-host-doubao-{version}-linux-amd64.tar.gz',host,prefix))
manifest={file.name:dict(sha256=hashlib.sha256(file.read_bytes()).hexdigest(),bytes=file.stat().st_size) for file in files}
(args.out_dir/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
(args.out_dir/'SHA256SUMS').write_text(''.join(f'{value["sha256"]}  {name}\n' for name,value in manifest.items()))
print(json.dumps({name:value['bytes'] for name,value in manifest.items()}))
