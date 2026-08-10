/* hash_oracle.c — dev-only helper that exposes the sqlite3_rsync hash
** engine for capturing golden test vectors.
**
** Build (from testdata/, or via generate-hash-golden-vectors.go):
**   cc -O1 -I ../references/sqlite-amalgamation-3530400 \
**      hash_oracle.c \
**      ../references/sqlite-amalgamation-3530400/sqlite3.c \
**      -o hash_oracle
**
** Usage: hash_oracle <hex-input> [<hex-input>...]
** Prints one line of 40 hex chars per input: the 160-bit hash of the
** decoded bytes.
*/
#define main sqlite3_rsync_main
#include "sqlite3_rsync.c"
#undef main

#include <stdio.h>
#include <stdlib.h>

static int hexval(char c){
  if( c>='0' && c<='9' ) return c-'0';
  if( c>='a' && c<='f' ) return c-'a'+10;
  if( c>='A' && c<='F' ) return c-'A'+10;
  return -1;
}

int main(int argc, char **argv){
  int iArg;
  for( iArg=1; iArg<argc; iArg++ ){
    const char *zHex = argv[iArg];
    unsigned char aData[4096];
    int nData = 0;
    int i;
    HashContext cx;
    unsigned char *pHash;
    for( i=0; zHex[i]; i += 2 ){
      int hi, lo;
      if( zHex[i+1]==0 ){
        fprintf(stderr, "odd-length hex at position %d\n", i);
        return 1;
      }
      if( nData >= (int)sizeof(aData) ){
        fprintf(stderr, "input too long (max %d bytes)\n", (int)sizeof(aData));
        return 1;
      }
      hi = hexval(zHex[i]);
      lo = hexval(zHex[i+1]);
      if( hi<0 || lo<0 ){
        fprintf(stderr, "invalid hex at position %d\n", i);
        return 1;
      }
      aData[nData++] = (unsigned char)((hi<<4)|lo);
    }
    HashInit(&cx, 160);
    HashUpdate(&cx, aData, (unsigned int)nData);
    pHash = HashFinal(&cx);
    for( i=0; i<20; i++ ) printf("%02x", pHash[i]);
    printf("\n");
  }
  return 0;
}
