type node struct{
	key,Val int
	nexts []*node
}
type Skiplist struct{
	head *node
}

func NewSkiplist() *Skiplist{
	return &Skiplist{
		head:&node{
			nexts:make([]*node,1)
		}
	}
}

//查询
func (s *Skiplist) Get(key int) (int,bool){
	if node:=s.search(key);node!=nil{
		return node.Val,true
	}
	return -1,false
} 
//查找方法，核心
func (s *Skiplist) search(key int) *node{
	move:=s.head
	for level:=len(move.nexts)-1; level>=0 ;level--{
		for move.nexts[level]!=nil && move.nexts[level].key<key{
			move=move.nexts[level]
		}
		if  move.nexts[level]!=nil &&move.nexts[level].key==key{
			return move.nexts[level]
		}  
	}
	return nil
}
//插入流程，骰子函数。
func (s *Skiplist) roll() int{
	level:=0
	for rand.Intn(2)>0{
		level++
	}
	return level
}
//插入
func (s *Skiplist) Put(key,val int){
	if node:=s.search(key);node!=nil{
		node.Val=val
		return
	}
	level:=s.roll()
	for len(s.head.nexts)-1<level{
		s.head.nexts=append(s.head.nexts,nil)
	}
	newNode:=&node{
		key:key
		Val:val
		nexts:make([]*node,level+1) //数字2，对应l0,l1,l2三层
	}
	move:=s.head
	for ;level>=0;level--{
		for move.nexts[level]!=nil && move.nexts[level].key<key{
			move=move.nexts[level]
		}
		newNode.nexts[level]=move.nexts[level]
		move.nexts[level]=newNode
	}
}
//删除
func (s *Skiplist) Delete(key int){
	if s.search(key)==nil{
		return
	}
	move:=s.head
	for level:=len(move.nexts)-1;level>=0;level--{
		for move.nexts[level]!=nil && move.nexts[level].key<key{
			move=move.nexts[level]
		}
		if move.nexts[level]==nil ||move.nexts[level].key>key{
			continue
		}
		move.nexts[level]=move.nexts[level].nexts[level]
	}

	dif:=0
	for level:=len(s.head.nexts)-1;level>0 &&s.head.nexts[level]==nil{
		dif++
	}
	if dif>0{
		s.head.nexts=s.head.nexts[:len(s.head.nexts)-dif]
	}

}
