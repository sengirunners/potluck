package boxport

// The parent retains the TCP socket and owns at most one read worker, one
// write worker and one bounded queued page in each direction. Independent
// workers let reads drain a peer while a backpressured write is pending.
//
// A write worker answers O once the kernel has its page, or after 2 s while
// it still holds the rest: the page is then buffered, and the worker keeps
// writing it for as long as the stream lives, because a peer may stop reading
// for any time. The next page queues behind it; after 2 s queued it is answered
// B (busy, nothing taken) and Exchange sends it again. A write worker that
// fails makes every later write answer E, and Close too, so a lost page shows
// at the next write or at Close; one lost after Close answered goes unreported.
// Close reaps the read worker at once and gives the write worker until the
// lease deadline or 60 s to deliver its acknowledged page. The 60 s idle
// timeout stretches to 600 s while a write worker holds a page (a host that
// left without Close does not keep the stream to lease end); it and the
// absolute lease deadline reap all owned workers. The TCP socket is non-blocking: a blocking
// send of a page larger than the kernel's free space hangs on some kernels
// (macOS) even with MSG_DONTWAIT.
const guestScript = `
use strict; use warnings; use IO::Socket::INET; use IO::Socket::UNIX; use IO::Select; use Socket qw(SOCK_STREAM AF_INET sockaddr_in inet_aton SOL_SOCKET SO_SNDBUF SO_RCVBUF MSG_DONTWAIT); use POSIX qw(setsid WNOHANG); use Fcntl qw(F_GETFL F_SETFL O_NONBLOCK); use Time::HiRes qw(time);
my ($op,$path,$port,$deadline)=@ARGV;
chdir($ENV{TMPDIR}||"/tmp") or die;
$path=".potluck-port-".$path;
$SIG{ALRM}=sub{die "socket timeout"}; alarm 6;
sub put { my($s,$b)=@_; while(length $b){my$n=syswrite($s,$b);die unless $n;substr($b,0,$n)=""} }
if($op eq 'open'){
 socket(my $tcp,AF_INET,SOCK_STREAM,0) or die;
 # Bound both kernel queues to a page (Linux reports at most twice the
 # requested size). Disable TCP auto-tuning so memory and backpressure do
 # not depend on the host's default multi-megabyte send buffer.
 setsockopt($tcp,SOL_SOCKET,SO_SNDBUF,pack('i',65536)) or die;
 setsockopt($tcp,SOL_SOCKET,SO_RCVBUF,pack('i',65536)) or die;
 for my$option(SO_SNDBUF,SO_RCVBUF){my$size=getsockopt($tcp,SOL_SOCKET,$option);die unless defined($size)&&unpack('i',$size)<=262144}
 connect($tcp,sockaddr_in($port,inet_aton('127.0.0.1'))) or die;
 fcntl($tcp,F_SETFL,fcntl($tcp,F_GETFL,0)|O_NONBLOCK) or die;
 my $srv=IO::Socket::UNIX->new(Type=>SOCK_STREAM,Local=>$path,Listen=>4) or die;
 chmod 0600,$path;
 my $pid=fork();die unless defined $pid;
 if($pid){print 'O';exit 0}
 close STDIN;close STDOUT;close STDERR;setsid();alarm 0;$SIG{PIPE}='IGNORE';
 my $last=time;my(%workers,%running,%pending);my($closed,$wfailed)=(0,0);
 eval {
 while(time<$deadline && (time-$last<60||($running{W}&&time-$last<600))){
  for my$p(keys %workers){if(waitpid($p,WNOHANG)==$p){$wfailed=1 if$workers{$p}eq'W'&&$?;delete$running{$workers{$p}};delete$workers{$p}}}
  for my$code(keys %pending){
   my($c,$data,$since)=@{$pending{$code}};
   if($running{$code}){if(time-$since>2){eval{put($c,'B')};close$c;delete$pending{$code}}next}
   if($code eq 'W'&&$wfailed){eval{put($c,'E')};close$c;delete$pending{$code};next}
   my$p=fork();die unless defined$p;
   if(!$p){
    close$srv;for my$q(values %pending){close$q->[0] if fileno($q->[0])!=fileno($c)}
    alarm 5;
    my$ok=eval{
     if($code eq 'W'){
      my$ack=time+2;
      while(length$data){
       if($c&&time>=$ack){eval{put($c,'O')};close$c;undef$c;alarm 0}
       my$wait=$c?$ack-time:1;$wait=0 if$wait<0;
       next unless IO::Select->new($tcp)->can_write($wait);
       my$n=send($tcp,$data,MSG_DONTWAIT);
       if(!defined$n){next if$!{EAGAIN}||$!{EWOULDBLOCK}||$!{EINTR};die}
       substr($data,0,$n)='';
      }
     }
     else{if(IO::Select->new($tcp)->can_read(0.2)){my$n=sysread($tcp,my$b,65536);if(!defined$n){die unless$!{EAGAIN}||$!{EWOULDBLOCK}||$!{EINTR};put($c,'O')}else{put($c,($n?'O':'F').$b)}}else{put($c,'O')}}
     1;
    };
    if($c&&$code eq 'W'){alarm 5;eval{put($c,$ok?'O':'E')}}
    close$c if$c;exit($ok?0:1);
   }
   $workers{$p}=$code;$running{$code}=$p;delete$pending{$code};close$c;
  }
  next unless IO::Select->new($srv)->can_read(0.02);
  my$c=$srv->accept() or next;$last=time;alarm 5;
  my$h='';while(length($h)<5){my$n=sysread($c,my$b,5-length($h));die unless$n;$h.=$b}
  my($code,$len)=unpack('aN',$h);die if$len>65536;
  my$data='';while(length($data)<$len){my$n=sysread($c,my$b,$len-length($data));die unless$n;$data.=$b}
  if($code eq 'C'){for my$p(keys %workers){if($workers{$p}eq'W'&&waitpid($p,WNOHANG)==$p){$wfailed=1 if$?;delete$running{W};delete$workers{$p}}}put($c,$wfailed?'E':'O');close$c;$closed=1;last}
  if(($code ne 'R' && $code ne 'W')||exists$pending{$code}||($code eq 'W'&&$wfailed)){put($c,'E');close$c}else{$pending{$code}=[$c,$data,time]}
  alarm 0;
 }
 };
 alarm 0;close$srv;unlink$path;close($_->[0]) for values %pending;
 for my$p(keys %workers){kill 'KILL',$p unless$closed&&$workers{$p}eq'W'}
 my$until=time+60;$until=$deadline if$deadline<$until;
 while(%workers){for my$p(keys %workers){delete$workers{$p} if waitpid($p,WNOHANG)==$p}last if time>=$until;select(undef,undef,undef,0.05) if %workers}
 kill 'KILL',keys %workers if %workers;waitpid($_,0) for keys %workers;
 close$tcp;exit 0;
}
my$c=IO::Socket::UNIX->new(Type=>SOCK_STREAM,Peer=>$path) or die;
my$data='';if($op eq 'write'){while(1){my$n=sysread(STDIN,my$b,65536);die unless defined$n;last unless$n;$data.=$b;die if length($data)>65536}}
my$code=$op eq 'read'?'R':$op eq 'write'?'W':'C';put($c,pack('aN',$code,length($data)).$data);shutdown($c,1);
my$out='';while(1){my$n=sysread($c,my$b,65537);die unless defined$n;last unless$n;$out.=$b;die if length($out)>65537}print$out;
`
